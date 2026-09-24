// Package guard is the session behind "snapvault mcp": it checkpoints a
// folder automatically while an agent works in it and exposes checkpoint,
// list, diff, and restore as MCP tools. Every restore first checkpoints the
// folder's current state, so an undo can itself be undone.
package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/mcp"
	"github.com/Hussain0327/snapvault/go/internal/repo"
)

const (
	// defaultListLimit is how many checkpoints list_checkpoints shows when
	// the agent does not ask for a number.
	defaultListLimit = 20
	// backoffFactor spaces automatic checkpoints at least this many scan
	// durations apart, so a large folder never keeps the machine busy.
	backoffFactor = 10
)

// Session owns one protected folder for the lifetime of an agent session.
// A mutex serializes the timer and tool calls.
type Session struct {
	mu       sync.Mutex
	repo     *repo.Repository
	interval time.Duration
	logger   *log.Logger
}

// New returns a session over r that checkpoints automatically every interval.
func New(r *repo.Repository, interval time.Duration) *Session {
	return &Session{repo: r, interval: interval, logger: log.New(os.Stderr, "snapvault mcp: ", log.LstdFlags)}
}

// Checkpoint records the folder if it changed since the last checkpoint,
// labeling it "auto: <reason>", and returns the current checkpoint id.
func (s *Session) Checkpoint(reason string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _, err := s.repo.SnapshotIfChanged("auto: " + reason)
	return id, err
}

// Run checkpoints the folder until ctx is cancelled, waiting at least the
// session interval between checkpoints and longer when scans are slow. A
// failed checkpoint (for example while a human runs a SnapVault command
// against the same store) is logged and retried on the next tick.
func (s *Session) Run(ctx context.Context) {
	wait := s.interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		start := time.Now()
		if _, err := s.Checkpoint("timer"); err != nil {
			s.logger.Printf("automatic checkpoint failed: %v", err)
		}
		wait = nextWait(s.interval, time.Since(start))
	}
}

// nextWait is the pause before the next automatic checkpoint.
func nextWait(interval, scan time.Duration) time.Duration {
	return max(interval, backoffFactor*scan)
}

// DefaultStoreDir is where a folder's checkpoints live unless --store says
// otherwise: $XDG_DATA_HOME/snapvault/stores/<hash>, falling back to
// ~/Library/Application Support on macOS and ~/.local/share elsewhere. The
// hash is of the folder's resolved path, so each folder gets its own store.
func DefaultStoreDir(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if runtime.GOOS == "darwin" {
			base = filepath.Join(home, "Library", "Application Support")
		} else {
			base = filepath.Join(home, ".local", "share")
		}
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(base, "snapvault", "stores", hex.EncodeToString(sum[:8])), nil
}

// Instructions tell the model how to use the tools.
const Instructions = "SnapVault checkpoints this folder automatically every few seconds and before every restore. " +
	"Call checkpoint with a short reason before a risky change (deleting, moving, or bulk-editing files). " +
	"To undo, use list_checkpoints and diff to find the right checkpoint, then restore it, " +
	"naming paths to undo only some files. Every restore reports a checkpoint that undoes it."

// Tools returns the session's MCP tools.
func (s *Session) Tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "checkpoint",
			Description: "Record the folder's current state now. Does nothing if nothing changed since the last checkpoint.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"Why you are checkpointing, e.g. 'before deleting build outputs'."}}}`),
			Handler:     s.checkpointTool,
		},
		{
			Name:        "list_checkpoints",
			Description: "List recent checkpoints, newest first, with id, time, and reason.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"description":"How many to show (default 20)."}}}`),
			ReadOnly:    true,
			Handler:     s.listTool,
		},
		{
			Name:        "diff",
			Description: "Show which paths differ between a checkpoint and the folder now, or between two checkpoints. Lines are 'A path' (added), 'M path' (modified), 'D path' (deleted).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","description":"Checkpoint id (7+ characters) or HEAD~n."},"to":{"type":"string","description":"Optional second checkpoint; omit to compare with the folder now."}},"required":["from"]}`),
			ReadOnly:    true,
			Handler:     s.diffTool,
		},
		{
			Name:        "restore",
			Description: "Restore paths (or, with no paths, the whole folder) to a checkpoint. The current state is checkpointed first and reported, so the restore can be undone.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"checkpoint":{"type":"string","description":"Checkpoint id (7+ characters) or HEAD~n."},"paths":{"type":"array","items":{"type":"string"},"description":"Folder-relative paths to restore; omit to restore everything."}},"required":["checkpoint"]}`),
			Destructive: true,
			Handler:     s.restoreTool,
		},
	}
}

func (s *Session) checkpointTool(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	message := strings.TrimSpace(in.Message)
	if message == "" {
		message = "checkpoint requested"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, created, err := s.repo.SnapshotIfChanged("agent: " + message)
	if err != nil {
		return "", err
	}
	if !created {
		return fmt.Sprintf("No changes since checkpoint %s.", short(id)), nil
	}
	return fmt.Sprintf("Created checkpoint %s.", short(id)), nil
}

func (s *Session) listTool(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if in.Limit < 1 {
		in.Limit = defaultListLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := s.repo.Head()
	if err != nil {
		return "", err
	}
	if head == "" {
		return "No checkpoints yet.", nil
	}
	history, err := s.repo.History("HEAD", in.Limit)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range history {
		fmt.Fprintf(&b, "%s  %s  %s\n", short(c.ID), c.Commit.Time.Local().Format(time.DateTime), c.Commit.Message)
	}
	return b.String(), nil
}

func (s *Session) diffTool(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if in.From == "" {
		return "", fmt.Errorf("from is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var changes []repo.Change
	var err error
	if in.To == "" {
		changes, err = s.repo.DiffWorking(in.From)
	} else {
		changes, err = s.repo.Diff(in.From, in.To)
	}
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "No changes.", nil
	}
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%c %s\n", c.Type.Status(), c.Path)
	}
	return b.String(), nil
}

func (s *Session) restoreTool(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Checkpoint string   `json:"checkpoint"`
		Paths      []string `json:"paths"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if in.Checkpoint == "" {
		return "", fmt.Errorf("checkpoint is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := s.repo.ResolveCommit(in.Checkpoint)
	if err != nil {
		return "", err
	}
	undo, _, err := s.repo.SnapshotIfChanged("pre-restore: before restoring " + short(target))
	if err != nil {
		return "", fmt.Errorf("could not checkpoint the current state, so nothing was restored: %w", err)
	}
	scope := "the whole folder"
	if len(in.Paths) > 0 {
		err = s.repo.RestorePaths(target, in.Paths)
		scope = strings.Join(in.Paths, ", ")
	} else {
		// The pre-restore checkpoint above makes the folder clean, so force
		// only skips a dirty check that can no longer fail for good reason.
		err = s.repo.Restore(target, "", true)
	}
	if err != nil {
		return "", fmt.Errorf("restore failed; the state before it is checkpoint %s: %w", short(undo), err)
	}
	return fmt.Sprintf("Restored %s to checkpoint %s. To undo this, restore checkpoint %s.",
		scope, short(target), short(undo)), nil
}

// short abbreviates an id the way the CLI does.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
