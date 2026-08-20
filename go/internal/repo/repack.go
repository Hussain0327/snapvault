package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/Hussain0327/snapvault/go/internal/delta"
	"github.com/Hussain0327/snapvault/go/internal/object"
)

// RepackStats reports what one Repack call did (or, under dryRun, would
// do): how many objects it rewrote and their total size before and after.
type RepackStats struct {
	RewrittenObjects int
	BeforeBytes      int64
	AfterBytes       int64
}

const (
	// repackMinDeltaCandidateSize is the smallest canonical blob size (header
	// plus payload) eligible to be delta-encoded or used as a delta base.
	// Below this, the envelope and instruction-stream overhead of a delta
	// can't plausibly beat storing the object whole.
	repackMinDeltaCandidateSize = 64

	// repackWindowSize bounds how many prior candidates (in sorted order) a
	// blob may pick a delta base from, matching the classic sliding-window
	// delta heuristic.
	repackWindowSize = 10

	// repackMaxChainDepth is the writer's self-imposed cap on a freshly
	// planned delta chain, stricter than the format's read-time cap so
	// chains this repack builds stay shallow and quick to resolve.
	repackMaxChainDepth = 10

	// repackMinSavingFraction is the smallest fractional reduction (versus
	// the object's current on-disk size) that justifies rewriting it.
	repackMinSavingFraction = 0.05
)

// Repack rewrites reachable objects into their smallest valid format v2
// encoding: a full zstd container, or (blobs only) a zstd-compressed delta
// against a nearby similarly-named blob. It never touches an object that
// isn't reachable from a ref, and it rewrites an object only when the
// smaller encoding beats its current on-disk bytes by at least 5%. Under
// dryRun it computes and reports the same plan without writing anything.
//
// The store package's own writer never produces container/delta objects
// (see store.go's "who writes what" note); Repack is the one place that
// does, so it builds and verifies the SVO2 delta envelope itself, mirroring
// the read side documented in store/container.go.
//
// Every rewritten object is decoded end to end and digest-checked in a
// temporary file before the atomic rename that replaces it, so the object
// on disk is valid at every instant. The repository lock is held for the
// whole operation.
func (r *Repository) Repack(dryRun bool) (RepackStats, error) {
	if r.version != maxFormatVersion {
		return RepackStats{}, errors.New(
			"repository is still format 1 (run 'snapvault upgrade' first)")
	}

	lock, err := acquireLock(filepath.Join(r.metadata, "lock"))
	if err != nil {
		return RepackStats{}, err
	}
	defer lock.close()

	trees, commits, blobHints, err := r.reachableObjects()
	if err != nil {
		return RepackStats{}, err
	}

	var stats RepackStats

	// Trees and commits are eligible for a full-zstd rewrite only, never a
	// delta; process them in a deterministic (sorted) order.
	fullOnly := append(append([]string{}, trees...), commits...)
	slices.Sort(fullOnly)
	for _, id := range fullOnly {
		if err := r.repackFullOnly(id, dryRun, &stats); err != nil {
			return RepackStats{}, err
		}
	}

	eligible, tooSmall, err := r.partitionBlobs(blobHints)
	if err != nil {
		return RepackStats{}, err
	}
	slices.Sort(tooSmall)
	for _, id := range tooSmall {
		if err := r.repackFullOnly(id, dryRun, &stats); err != nil {
			return RepackStats{}, err
		}
	}

	var window []windowEntry
	for _, c := range eligible {
		depth, err := r.repackBlob(c.id, window, dryRun, &stats)
		if err != nil {
			return RepackStats{}, err
		}
		window = append(window, windowEntry{id: c.id, depth: depth})
		if len(window) > repackWindowSize {
			window = window[len(window)-repackWindowSize:]
		}
	}

	return stats, nil
}

// blobCandidate is one delta-eligible blob awaiting a repack decision, in
// the design's candidate order: (name hint, canonical size descending, id).
type blobCandidate struct {
	id       string
	nameHint string
	size     int64
}

// windowEntry is a previously decided delta-eligible blob, kept around so a
// later candidate can consider it as a delta base within the sliding
// window; depth is that blob's resulting on-disk delta chain depth after
// this repack's decision (0 for a full or kept-full object).
type windowEntry struct {
	id    string
	depth int
}

// partitionBlobs splits every reachable blob into delta candidates (at
// least repackMinDeltaCandidateSize canonical bytes), sorted into the
// design's candidate order, and everything else (handled like a tree: full
// or keep, never delta).
func (r *Repository) partitionBlobs(hints map[string]string) (eligible []blobCandidate, tooSmall []string, err error) {
	for id, hint := range hints {
		_, size, err := r.canonicalSize(id)
		if err != nil {
			return nil, nil, err
		}
		if size >= repackMinDeltaCandidateSize {
			eligible = append(eligible, blobCandidate{id: id, nameHint: hint, size: size})
		} else {
			tooSmall = append(tooSmall, id)
		}
	}
	slices.SortFunc(eligible, func(a, b blobCandidate) int {
		if c := object.CompareNames(a.nameHint, b.nameHint); c != 0 {
			return c
		}
		if a.size != b.size {
			if a.size > b.size {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	return eligible, tooSmall, nil
}

// reachableObjects walks every commit reachable from the current ref (there
// is only ever one, refs/heads/main, today) and every tree it roots,
// returning the deduplicated tree and commit ids and, for every reachable
// blob, the name of the most recent tree entry found to reference it.
//
// Commits are discovered depth-first from HEAD (matching History's order:
// newest first, first-parent line first), then trees are walked oldest
// commit to newest so a blob's name hint reflects its current path rather
// than a name it has since been renamed away from.
func (r *Repository) reachableObjects() (trees []string, commits []string, blobHints map[string]string, err error) {
	blobHints = make(map[string]string)
	head, err := r.Head()
	if err != nil {
		return nil, nil, nil, err
	}
	if head == "" {
		return nil, nil, blobHints, nil
	}

	visited := make(map[string]bool)
	pending := []string{head}
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		commits = append(commits, id)
		commit, err := r.ReadCommit(id)
		if err != nil {
			return nil, nil, nil, err
		}
		for i := len(commit.Parents) - 1; i >= 0; i-- {
			pending = append(pending, commit.Parents[i])
		}
	}

	visitedTrees := make(map[string]bool)
	for i := len(commits) - 1; i >= 0; i-- {
		commit, err := r.ReadCommit(commits[i])
		if err != nil {
			return nil, nil, nil, err
		}
		if err := r.walkReachableTree(commit.TreeID, visitedTrees, &trees, blobHints); err != nil {
			return nil, nil, nil, err
		}
	}
	return trees, commits, blobHints, nil
}

func (r *Repository) walkReachableTree(
	treeID string, visited map[string]bool, trees *[]string, blobHints map[string]string,
) error {
	if visited[treeID] {
		return nil
	}
	visited[treeID] = true
	*trees = append(*trees, treeID)

	tree, err := r.readTree(treeID)
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries() {
		if entry.Kind == object.KindDirectory {
			if err := r.walkReachableTree(entry.ObjectID, visited, trees, blobHints); err != nil {
				return err
			}
			continue
		}
		blobHints[entry.ObjectID] = entry.Name
	}
	return nil
}

// repackFullOnly decides between keeping id's current bytes and rewriting it
// as a full-zstd container; it is used for trees, commits, and blobs too
// small to be worth delta-encoding.
func (r *Repository) repackFullOnly(id string, dryRun bool, stats *RepackStats) error {
	typ, payload, err := r.store.Get(id)
	if err != nil {
		return err
	}
	canonical := canonicalBytes(typ, payload)
	keepSize, err := r.onDiskSize(id)
	if err != nil {
		return err
	}
	fullBytes, err := encodeContainerFull(canonical)
	if err != nil {
		return err
	}
	fullSize := int64(len(fullBytes))
	if !meetsSavingFloor(keepSize, fullSize) {
		return nil
	}

	stats.RewrittenObjects++
	stats.BeforeBytes += keepSize
	stats.AfterBytes += fullSize
	if dryRun {
		return nil
	}
	return r.writeRepackedObject(id, typ, fullBytes, nil)
}

// repackBlob decides id's smallest valid encoding among keeping its current
// bytes, a full-zstd container, and the best delta against a base drawn
// from window, then reports the on-disk delta chain depth id ends up with
// (0 unless it is written, or already stored, as a delta) so a later
// candidate's window entry reflects it correctly.
func (r *Repository) repackBlob(id string, window []windowEntry, dryRun bool, stats *RepackStats) (int, error) {
	typ, payload, err := r.store.Get(id)
	if err != nil {
		return 0, err
	}
	if typ != object.TypeBlob {
		return 0, fmt.Errorf("tree entry references a non-blob object as a file: %s", id)
	}
	canonical := canonicalBytes(typ, payload)
	keepSize, err := r.onDiskSize(id)
	if err != nil {
		return 0, err
	}

	fullBytes, err := encodeContainerFull(canonical)
	if err != nil {
		return 0, err
	}
	bestSize := int64(len(fullBytes))
	bestIsDelta := false
	var bestDeltaBytes []byte
	var bestBaseID string
	bestDepth := 0

	for _, w := range window {
		if w.depth+1 > repackMaxChainDepth {
			continue
		}
		// A window entry's depth alone doesn't say what's on its chain: a
		// kept object's depth comes from the chain already on disk, and
		// that chain can point straight back at id (a rename, or any edit
		// that reorders partitionBlobs' candidate list can do this).
		// Basing id on w.id in that case would make id's new bytes and
		// w.id's existing bytes mutually dependent, so refuse the base
		// before ever encoding against it.
		onChain, err := r.onDiskChainReaches(w.id, id)
		if err != nil {
			return 0, err
		}
		if onChain {
			continue
		}
		baseType, basePayload, err := r.store.Get(w.id)
		if err != nil {
			return 0, err
		}
		baseCanonical := canonicalBytes(baseType, basePayload)
		instructions := delta.Encode(baseCanonical, canonical)
		deltaBytes, err := encodeContainerDelta(w.id, instructions)
		if err != nil {
			return 0, err
		}
		if int64(len(deltaBytes)) < bestSize {
			bestSize = int64(len(deltaBytes))
			bestIsDelta = true
			bestDeltaBytes = deltaBytes
			bestBaseID = w.id
			bestDepth = w.depth + 1
		}
	}

	if !meetsSavingFloor(keepSize, bestSize) {
		depth, err := r.onDiskDepth(id)
		if err != nil {
			return 0, err
		}
		return depth, nil
	}

	stats.RewrittenObjects++
	stats.BeforeBytes += keepSize
	stats.AfterBytes += bestSize
	if !bestIsDelta {
		if !dryRun {
			if err := r.writeRepackedObject(id, typ, fullBytes, nil); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	if !dryRun {
		baseType, basePayload, err := r.store.Get(bestBaseID)
		if err != nil {
			return 0, err
		}
		if err := r.writeRepackedObject(id, typ, bestDeltaBytes, canonicalBytes(baseType, basePayload)); err != nil {
			return 0, err
		}
	}
	return bestDepth, nil
}

// canonicalSize returns id's type and canonical (header plus payload) byte
// count, the size the >= 64 byte delta-eligibility floor is measured
// against.
func (r *Repository) canonicalSize(id string) (object.Type, int64, error) {
	typ, payload, err := r.store.Get(id)
	if err != nil {
		return 0, 0, err
	}
	return typ, int64(len(canonicalBytes(typ, payload))), nil
}

func canonicalBytes(t object.Type, payload []byte) []byte {
	return append(object.Header(t, int64(len(payload))), payload...)
}

// meetsSavingFloor reports whether newSize is at least
// repackMinSavingFraction smaller than oldSize.
func meetsSavingFloor(oldSize, newSize int64) bool {
	if oldSize <= 0 || newSize >= oldSize {
		return false
	}
	return float64(oldSize-newSize) >= float64(oldSize)*repackMinSavingFraction
}

// objectPath returns id's on-disk path under the repository's sharded
// object layout (two hex characters, then the remaining 62), the same
// layout docs/FORMAT.md defines and the store package uses internally.
func (r *Repository) objectPath(id string) string {
	return filepath.Join(r.metadata, "objects", id[:2], id[2:])
}

func (r *Repository) onDiskSize(id string) (int64, error) {
	info, err := os.Stat(r.objectPath(id))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// The wire format below mirrors store/container.go's SVO2 container
// envelope exactly: magic, kind, codec, an optional 32-byte raw delta base
// id, then a codec stream of either the canonical bytes (kindFull) or a
// delta instruction stream (kindDelta). The store package writes and reads
// container/full itself; Repack is the only writer of container/delta (see
// store.go's "who writes what" note), so it builds and verifies that form
// here rather than exposing a store-internal writer.
const (
	repackContainerMagic      = "SVO2"
	repackKindFull       byte = 0x01
	repackKindDelta      byte = 0x02
	repackCodecZstd      byte = 0x02
	repackBaseIDLen           = 32

	// repackMaxDecodedBytes bounds a verification decode, matching the
	// store's own cap on a reconstructed canonical object.
	repackMaxDecodedBytes = 256 << 20
)

// encodeContainerFull renders canonical as a container/full/zstd object
// body, ready to write to an object file.
func encodeContainerFull(canonical []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(repackContainerMagic)
	buf.WriteByte(repackKindFull)
	buf.WriteByte(repackCodecZstd)
	if err := zstdCompress(&buf, canonical); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeContainerDelta renders instructions (already Encode'd against
// baseID's canonical bytes) as a container/delta/zstd object body.
func encodeContainerDelta(baseID string, instructions []byte) ([]byte, error) {
	rawBaseID, err := hex.DecodeString(baseID)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(repackContainerMagic)
	buf.WriteByte(repackKindDelta)
	buf.WriteByte(repackCodecZstd)
	buf.Write(rawBaseID)
	if err := zstdCompress(&buf, instructions); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func zstdCompress(dst *bytes.Buffer, data []byte) error {
	zw, err := zstd.NewWriter(dst)
	if err != nil {
		return err
	}
	if _, err := zw.Write(data); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

func zstdDecompress(data []byte) ([]byte, error) {
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	capped := io.LimitReader(zr, repackMaxDecodedBytes+1)
	out, err := io.ReadAll(capped)
	if err != nil {
		return nil, err
	}
	if len(out) > repackMaxDecodedBytes {
		return nil, fmt.Errorf("decoded object exceeds the %d byte cap", repackMaxDecodedBytes)
	}
	return out, nil
}

// writeRepackedObject writes candidateBytes (a full container-body result
// of encodeContainerFull or encodeContainerDelta) to a temporary file,
// fully decodes and digest-checks that file against id, and only then
// atomically replaces the existing object file. baseCanonical is nil for a
// full-form candidate and the delta base's canonical bytes otherwise.
func (r *Repository) writeRepackedObject(id string, typ object.Type, candidateBytes []byte, baseCanonical []byte) error {
	dir := filepath.Dir(r.objectPath(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "tmp-repack-*.object")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(candidateBytes); err != nil {
		tmp.Close()
		return err
	}
	// Sync before close: the design's crash-safety claim ("every object file
	// is individually valid at all times") only holds if the replacement's
	// data blocks are durable before the rename that discards the original
	// good bytes, not just its directory entry.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := verifyRepackedObjectFile(tmpPath, id, typ, baseCanonical); err != nil {
		return fmt.Errorf("repack produced an invalid object for %s: %w", id, err)
	}

	if err := os.Rename(tmpPath, r.objectPath(id)); err != nil {
		return err
	}
	tmpPath = ""
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

// syncDir fsyncs a directory so a preceding rename within it is durable, not
// just ordered; without this a crash can still leave the rename's data
// blocks unflushed even though the directory entry itself landed.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// verifyRepackedObjectFile fully decodes the container object at path,
// resolving a delta against baseCanonical, and checks the result's SHA-256
// digest and declared type against id and typ before the caller trusts it.
func verifyRepackedObjectFile(path string, id string, typ object.Type, baseCanonical []byte) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	prefixLen := len(repackContainerMagic) + 2
	if len(raw) < prefixLen || string(raw[:len(repackContainerMagic)]) != repackContainerMagic {
		return errors.New("missing container magic")
	}
	kind, codec := raw[len(repackContainerMagic)], raw[len(repackContainerMagic)+1]
	if codec != repackCodecZstd {
		return fmt.Errorf("unexpected codec byte %#x", codec)
	}
	rest := raw[prefixLen:]

	var canonical []byte
	switch kind {
	case repackKindFull:
		canonical, err = zstdDecompress(rest)
		if err != nil {
			return err
		}
	case repackKindDelta:
		if len(rest) < repackBaseIDLen {
			return errors.New("truncated delta base id")
		}
		instructions, err := zstdDecompress(rest[repackBaseIDLen:])
		if err != nil {
			return err
		}
		canonical, err = delta.Apply(baseCanonical, instructions)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown container kind byte %#x", kind)
	}

	sum := sha256.Sum256(canonical)
	if actual := hex.EncodeToString(sum[:]); actual != id {
		return fmt.Errorf("digest mismatch: computed %s", actual)
	}
	gotType, _, err := parseCanonicalEnvelope(canonical)
	if err != nil {
		return err
	}
	if gotType != typ {
		return fmt.Errorf("type mismatch: got %s, want %s", gotType.Token(), typ.Token())
	}
	return nil
}

// parseCanonicalEnvelope splits fully-decoded canonical bytes into their
// declared type and payload, matching the envelope rules the store package
// enforces on read.
func parseCanonicalEnvelope(canonical []byte) (object.Type, []byte, error) {
	nul := bytes.IndexByte(canonical, 0)
	if nul <= 0 {
		return 0, nil, errors.New("malformed object header")
	}
	header := canonical[:nul]
	sep := bytes.IndexByte(header, ' ')
	if sep <= 0 || sep == len(header)-1 {
		return 0, nil, errors.New("malformed object header")
	}
	t, err := object.TypeFromToken(string(header[:sep]))
	if err != nil {
		return 0, nil, err
	}
	size, err := strconv.ParseInt(string(header[sep+1:]), 10, 64)
	if err != nil || size < 0 {
		return 0, nil, errors.New("malformed object size")
	}
	payload := canonical[nul+1:]
	if int64(len(payload)) != size {
		return 0, nil, fmt.Errorf("payload is %d bytes, header declares %d", len(payload), size)
	}
	return t, payload, nil
}

// onDiskDepth reports id's current on-disk delta chain depth by following
// container/delta base references without decompressing anything: 0 for a
// legacy or container/full object, or one more than its base's depth for a
// container/delta object. It only ever needs to look past a header few
// bytes long, so it stays cheap even though it can recurse.
func (r *Repository) onDiskDepth(id string) (int, error) {
	depth := 0
	current := id
	seen := make(map[string]bool)
	for {
		if seen[current] {
			return 0, fmt.Errorf("object %s has a cyclic delta base chain on disk", id)
		}
		seen[current] = true

		kind, baseID, err := readContainerKind(r.objectPath(current))
		if err != nil {
			return 0, err
		}
		if kind != repackKindDelta {
			return depth, nil
		}
		depth++
		current = baseID
	}
}

// onDiskChainReaches reports whether target appears anywhere on the on-disk
// delta base chain starting at id: id's base, that base's base, and so on.
// It exists to stop repack from choosing a delta base whose chain already
// passes through the object currently being decided, which would otherwise
// leave two (or more) object files mutually dependent and undecodable by
// any implementation. Like onDiskDepth, it only ever reads a header few
// bytes long per hop.
func (r *Repository) onDiskChainReaches(id, target string) (bool, error) {
	current := id
	seen := make(map[string]bool)
	for {
		if current == target {
			return true, nil
		}
		if seen[current] {
			return false, fmt.Errorf("object %s has a cyclic delta base chain on disk", id)
		}
		seen[current] = true

		kind, baseID, err := readContainerKind(r.objectPath(current))
		if err != nil {
			return false, err
		}
		if kind != repackKindDelta {
			return false, nil
		}
		current = baseID
	}
}

// readContainerKind peeks at an object file's header, reporting kind 0 (an
// unused byte value, distinct from both repackKindFull and repackKindDelta)
// for anything that isn't a container/delta object: a legacy object, or a
// container/full one.
func readContainerKind(path string) (kind byte, baseID string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	header := make([]byte, len(repackContainerMagic)+2)
	if _, err := io.ReadFull(f, header); err != nil {
		return 0, "", nil // shorter than a container header: not a delta.
	}
	if string(header[:len(repackContainerMagic)]) != repackContainerMagic {
		return 0, "", nil // legacy zlib object.
	}
	if header[len(repackContainerMagic)] != repackKindDelta {
		return 0, "", nil
	}
	rawBaseID := make([]byte, repackBaseIDLen)
	if _, err := io.ReadFull(f, rawBaseID); err != nil {
		return 0, "", fmt.Errorf("object is corrupt: %s: truncated delta base id", path)
	}
	return repackKindDelta, hex.EncodeToString(rawBaseID), nil
}
