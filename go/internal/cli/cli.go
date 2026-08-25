// Package cli implements the SnapVault command-line interface with the same
// commands, output, and exit codes as the Java reference implementation,
// plus a --workers flag bounding the concurrent hashing pool.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Hussain0327/snapvault/go/internal/eval"
	"github.com/Hussain0327/snapvault/go/internal/model2vec"
	"github.com/Hussain0327/snapvault/go/internal/object"
	"github.com/Hussain0327/snapvault/go/internal/repo"
	"github.com/Hussain0327/snapvault/go/internal/search"
)

const (
	defaultLogLimit  = 50
	defaultFindLimit = 5

	// defaultStaticModel is what "--embedder static", with no explicit
	// model name, resolves to.
	defaultStaticModel = "potion-base-8M"

	// defaultEvalK and defaultEvalBeta are "eval run"'s -k and --beta
	// defaults, matching the eval spec's Aggregate defaults exactly so an
	// unqualified "eval run" reports the same headline metric
	// (fbeta@10) that tests/golden/search/baseline.json pins.
	defaultEvalK    = 10
	defaultEvalBeta = 2.0
	// defaultEvalPct is "eval run"'s --pct default: evaluate every question.
	defaultEvalPct = 100
	// defaultEvalPerFile is "eval generate"'s --per-file default.
	defaultEvalPerFile = 2
)

// evalOllamaBaseURL is the local Ollama server "eval generate" talks to.
// It is a package-level var, not a constant, purely so cli_test.go can
// redirect it at an httptest.Server for TestEvalGenerateAgainstHTTPTestServer
// — the spec's CLI surface has no --base-url flag for eval generate (every
// other Ollama-backed command hardcodes localhost too), so there is no
// flag-driven way to point it elsewhere.
var evalOllamaBaseURL = "http://localhost:11434"

// usageError reports a mistake in how the command was invoked; it exits
// with status 2 and a pointer at the help text.
type usageError struct {
	message string
}

func (e usageError) Error() string { return e.message }

// Run executes one CLI invocation and returns its exit code.
func Run(args []string, out, errOut io.Writer, workdir string) int {
	err := execute(args, out, errOut, workdir)
	if err == nil {
		return 0
	}
	var usage usageError
	if errors.As(err, &usage) {
		fmt.Fprintln(errOut, "error: "+usage.message)
		fmt.Fprintln(errOut, "Run 'snapvault help' for usage.")
		return 2
	}
	fmt.Fprintln(errOut, "error: "+describe(err))
	return 1
}

func execute(args []string, out, errOut io.Writer, workdir string) error {
	directory, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	i := 0
	for i < len(args) && args[i] == "-C" {
		if i+1 >= len(args) {
			return usageError{"-C requires a directory"}
		}
		directory = resolve(directory, args[i+1])
		i += 2
	}
	if i >= len(args) {
		printUsage(out)
		return nil
	}

	command := strings.ToLower(args[i])
	rest := args[i+1:]
	switch command {
	case "init":
		return runInit(out, directory, rest)
	case "snapshot", "commit":
		return runSnapshot(out, directory, rest)
	case "log":
		return runLog(out, directory, rest)
	case "diff":
		return runDiff(out, directory, rest)
	case "restore":
		return runRestore(out, directory, rest)
	case "upgrade":
		return runUpgrade(out, directory, rest)
	case "repack":
		return runRepack(out, directory, rest)
	case "index":
		return runIndex(out, directory, rest)
	case "find":
		return runFind(out, errOut, directory, rest)
	case "model":
		return runModel(out, errOut, rest)
	case "eval":
		return runEval(out, errOut, directory, rest)
	case "help", "--help", "-h":
		if len(rest) > 0 {
			return usageError{"help does not accept arguments"}
		}
		printUsage(out)
		return nil
	case "version", "--version":
		if len(rest) > 0 {
			return usageError{"version does not accept arguments"}
		}
		fmt.Fprintln(out, "SnapVault 1.0.0")
		return nil
	default:
		return usageError{"unknown command: " + command}
	}
}

func runInit(out io.Writer, directory string, args []string) error {
	if len(args) > 1 {
		return usageError{"init accepts at most one directory"}
	}
	target := directory
	if len(args) == 1 {
		target = resolve(directory, args[0])
	}
	r, err := repo.Init(target)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Initialized empty SnapVault repository in "+r.Metadata())
	return nil
}

func runSnapshot(out io.Writer, directory string, args []string) error {
	message := "Snapshot"
	workers := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-m" || arg == "--message":
			if i++; i >= len(args) {
				return usageError{arg + " requires a message"}
			}
			message = args[i]
		case strings.HasPrefix(arg, "--message="):
			message = arg[len("--message="):]
		case arg == "--workers":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			n, err := positiveInt(args[i], "snapshot workers")
			if err != nil {
				return err
			}
			workers = n
		case strings.HasPrefix(arg, "--workers="):
			n, err := positiveInt(arg[len("--workers="):], "snapshot workers")
			if err != nil {
				return err
			}
			workers = n
		default:
			return usageError{"unexpected snapshot argument: " + arg}
		}
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	r.SetWorkers(workers)
	commitID, err := r.Snapshot(message)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Snapshot "+abbreviate(commitID)+" "+strings.TrimSpace(message))
	return nil
}

func runLog(out io.Writer, directory string, args []string) error {
	oneline := false
	limit := defaultLogLimit
	revision := "HEAD"
	revisionSet := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--oneline":
			oneline = true
		case arg == "--limit" || arg == "-n":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			n, err := positiveInt(args[i], "log limit")
			if err != nil {
				return err
			}
			limit = n
		case strings.HasPrefix(arg, "--limit="):
			n, err := positiveInt(arg[len("--limit="):], "log limit")
			if err != nil {
				return err
			}
			limit = n
		case strings.HasPrefix(arg, "-"):
			return usageError{"unknown log option: " + arg}
		case !revisionSet:
			revision, revisionSet = arg, true
		default:
			return usageError{"log accepts at most one starting snapshot"}
		}
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	head, err := r.Head()
	if err != nil {
		return err
	}
	if head == "" {
		fmt.Fprintln(out, "No snapshots yet.")
		return nil
	}

	history, err := r.History(revision, limit)
	if err != nil {
		return err
	}
	for _, info := range history {
		if oneline {
			fmt.Fprintln(out, abbreviate(info.ID)+" "+firstLine(info.Commit.Message))
			continue
		}
		fmt.Fprintln(out, "commit "+info.ID)
		for _, parent := range info.Commit.Parents {
			fmt.Fprintln(out, "Parent: "+parent)
		}
		fmt.Fprintln(out, "Date:   "+formatJavaTimestamp(info.Commit.Time.In(time.Local)))
		fmt.Fprintln(out)
		for _, line := range messageLines(info.Commit.Message) {
			fmt.Fprintln(out, "    "+line)
		}
		fmt.Fprintln(out)
	}
	return nil
}

func runDiff(out io.Writer, directory string, args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return usageError{"unknown diff option: " + arg}
		}
	}
	if len(args) > 2 {
		return usageError{"diff accepts at most two snapshot revisions"}
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	var changes []repo.Change
	switch len(args) {
	case 0:
		changes, err = r.DiffWorkingFromHead()
	case 1:
		changes, err = r.DiffWorking(args[0])
	default:
		changes, err = r.Diff(args[0], args[1])
	}
	if err != nil {
		return err
	}

	if len(changes) == 0 {
		fmt.Fprintln(out, "No changes.")
		return nil
	}
	for _, change := range changes {
		suffix := ""
		if change.Entry().Kind == object.KindDirectory {
			suffix = "/"
		}
		fmt.Fprintf(out, "%c\t%s%s\n", change.Type.Status(), printablePath(change.Path), suffix)
	}
	noun := " changes"
	if len(changes) == 1 {
		noun = " change"
	}
	fmt.Fprintln(out, strconv.Itoa(len(changes))+noun)
	return nil
}

func runRestore(out io.Writer, directory string, args []string) error {
	force := false
	target := ""
	revision := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force" || arg == "-f":
			force = true
		case arg == "--to":
			if i++; i >= len(args) {
				return usageError{"--to requires a directory"}
			}
			target = resolve(directory, args[i])
		case strings.HasPrefix(arg, "--to="):
			target = resolve(directory, arg[len("--to="):])
		case strings.HasPrefix(arg, "-"):
			return usageError{"unknown restore option: " + arg}
		case revision == "":
			revision = arg
		default:
			return usageError{"restore requires exactly one snapshot revision"}
		}
	}
	if revision == "" {
		return usageError{"restore requires a snapshot revision"}
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	resolved, err := r.ResolveCommit(revision)
	if err != nil {
		return err
	}
	if err := r.Restore(resolved, target, force); err != nil {
		return err
	}
	restoredTo := target
	if restoredTo == "" {
		restoredTo = r.Root()
	}
	fmt.Fprintln(out, "Restored "+abbreviate(resolved)+" to "+restoredTo)
	return nil
}

func runUpgrade(out io.Writer, directory string, args []string) error {
	if len(args) > 0 {
		return usageError{"upgrade accepts no arguments"}
	}
	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	upgraded, err := r.Upgrade()
	if err != nil {
		return err
	}
	if !upgraded {
		fmt.Fprintln(out, "repository is already format 2")
		return nil
	}
	fmt.Fprintln(out, "Upgraded repository to format 2")
	return nil
}

func runRepack(out io.Writer, directory string, args []string) error {
	dryRun := false
	for _, arg := range args {
		if arg != "--dry-run" {
			return usageError{"unknown repack option: " + arg}
		}
		dryRun = true
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	stats, err := r.Repack(dryRun)
	if err != nil {
		return err
	}
	if stats.RewrittenObjects == 0 {
		fmt.Fprintln(out, "nothing to repack.")
		return nil
	}

	verb := "repacked"
	if dryRun {
		verb = "would repack"
	}
	var percent float64
	if stats.BeforeBytes > 0 {
		percent = float64(stats.BeforeBytes-stats.AfterBytes) / float64(stats.BeforeBytes) * 100
	}
	fmt.Fprintf(out, "%s %d objects: %s -> %s (%.0f%% smaller)\n",
		verb, stats.RewrittenObjects, humanBytes(stats.BeforeBytes), humanBytes(stats.AfterBytes), percent)
	return nil
}

func runIndex(out io.Writer, directory string, args []string) error {
	embedderArg := "builtin"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--embedder":
			if i++; i >= len(args) {
				return usageError{arg + " requires a value"}
			}
			embedderArg = args[i]
		case strings.HasPrefix(arg, "--embedder="):
			embedderArg = arg[len("--embedder="):]
		default:
			return usageError{"unexpected index argument: " + arg}
		}
	}
	embedder, err := parseEmbedderFlag(embedderArg)
	if err != nil {
		return err
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	stats, err := r.Index(embedder, search.DefaultPipeline())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "indexed %d blobs (%d chunks) with %s\n", stats.Blobs, stats.Chunks, embedder.ID())
	if stats.Skipped > 0 {
		fmt.Fprintf(out, "skipped %d blobs with no extractable text\n", stats.Skipped)
	}
	return nil
}

// parseEmbedderFlag translates an "index --embedder" value into the
// embedder it names: "builtin" for the lexical embedder, "static" or
// "static:<name>" for a locally installed Model2Vec model (bare "static" is
// an alias for defaultStaticModel), or "ollama:<model>" for a local Ollama
// server.
func parseEmbedderFlag(value string) (search.Embedder, error) {
	if value == "builtin" {
		return search.LexicalEmbedder{}, nil
	}
	if value == "static" {
		value = "static:" + defaultStaticModel
	}
	if name, ok := strings.CutPrefix(value, "static:"); ok && name != "" {
		return search.NewStaticEmbedder(name)
	}
	if model, ok := strings.CutPrefix(value, "ollama:"); ok && model != "" {
		return search.NewOllamaEmbedder(model), nil
	}
	return nil, usageError{"unknown embedder: " + value}
}

func runFind(out, errOut io.Writer, directory string, args []string) error {
	limit := defaultFindLimit
	query := ""
	querySet := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--limit" || arg == "-n":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			n, err := positiveInt(args[i], "find limit")
			if err != nil {
				return err
			}
			limit = n
		case strings.HasPrefix(arg, "--limit="):
			n, err := positiveInt(arg[len("--limit="):], "find limit")
			if err != nil {
				return err
			}
			limit = n
		case strings.HasPrefix(arg, "-"):
			return usageError{"unknown find option: " + arg}
		case !querySet:
			query, querySet = arg, true
		default:
			return usageError{"find accepts exactly one query"}
		}
	}
	if !querySet || strings.TrimSpace(query) == "" {
		return usageError{"find requires a query"}
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}
	p := search.DefaultPipeline()
	s, err := r.OpenSearcher(p)
	if err != nil {
		return err
	}
	defer s.Close()
	if s.IndexHash() != p.IndexHash() {
		fmt.Fprintln(errOut,
			"note: index was built with different chunking settings; run 'snapvault index' to rebuild")
	}

	results, err := s.Find(query, limit)
	if err != nil {
		return err
	}
	for _, res := range results {
		fmt.Fprintln(out, abbreviate(res.BlobID)+"  "+res.Path+
			"  (snapshot: \""+res.Message+"\", "+abbreviate(res.CommitID)+")")
		fmt.Fprintln(out, "    "+printableSnippet(res.Snippet))
	}
	return nil
}

// runModel dispatches "model pull <name>" and "model list".
func runModel(out, errOut io.Writer, args []string) error {
	if len(args) == 0 {
		return usageError{"model requires a subcommand: pull or list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "pull":
		return runModelPull(errOut, rest)
	case "list":
		return runModelList(out, rest)
	default:
		return usageError{"unknown model subcommand: " + sub}
	}
}

// runModelPull downloads a registered model's files, streaming progress to
// errOut. It is the only code path in the CLI that opens a network
// connection.
func runModelPull(errOut io.Writer, args []string) error {
	if len(args) != 1 {
		return usageError{"model pull requires exactly one model name"}
	}
	name := args[0]
	spec, ok := model2vec.Registry[name]
	if !ok {
		return usageError{"unknown model: " + name}
	}
	dir, err := model2vec.Dir(name)
	if err != nil {
		return err
	}
	if err := model2vec.Pull(context.Background(), spec, "", dir, errOut); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "installed %s in %s\n", name, dir)
	return nil
}

// runModelList prints every registered model's install status: installed,
// not installed, or corrupt (present but failing model2vec.Verify), and the
// directory it lives in or would be installed into.
func runModelList(out io.Writer, args []string) error {
	if len(args) > 0 {
		return usageError{"model list accepts no arguments"}
	}
	names := make([]string, 0, len(model2vec.Registry))
	for name := range model2vec.Registry {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		spec := model2vec.Registry[name]
		dir, err := model2vec.Dir(name)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s  %s  %s\n", name, modelStatus(spec, dir), dir)
	}
	return nil
}

// modelStatus reports whether spec's files are installed, absent
// (not installed), or present but failing verification (corrupt).
func modelStatus(spec model2vec.Spec, dir string) string {
	if err := model2vec.Verify(spec, dir); err == nil {
		return "installed"
	}
	for _, f := range spec.Files {
		if _, err := os.Stat(filepath.Join(dir, f.Name)); err != nil {
			return "not installed"
		}
	}
	return "corrupt"
}

// runEval dispatches "eval run" and "eval generate".
func runEval(out, errOut io.Writer, directory string, args []string) error {
	if len(args) == 0 {
		return usageError{"eval requires a subcommand: run or generate"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run":
		return runEvalRun(out, directory, rest)
	case "generate":
		return runEvalGenerate(errOut, directory, rest)
	default:
		return usageError{"unknown eval subcommand: " + sub}
	}
}

// runEvalRun implements "eval run --questions <file> [--corpus <dir>]
// [--embedder X] [-k n] [--beta f] [--pct n] [--json]": it loads and
// samples the question file, opens or builds the repository to evaluate
// against, and prints eval.Run's Report as a table or, with --json, as
// indented JSON.
func runEvalRun(out io.Writer, directory string, args []string) error {
	questionsPath := ""
	corpusDir := ""
	embedderArg := "builtin"
	k := defaultEvalK
	beta := defaultEvalBeta
	pct := defaultEvalPct
	jsonOut := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--questions":
			if i++; i >= len(args) {
				return usageError{arg + " requires a value"}
			}
			questionsPath = args[i]
		case strings.HasPrefix(arg, "--questions="):
			questionsPath = arg[len("--questions="):]
		case arg == "--corpus":
			if i++; i >= len(args) {
				return usageError{arg + " requires a value"}
			}
			corpusDir = args[i]
		case strings.HasPrefix(arg, "--corpus="):
			corpusDir = arg[len("--corpus="):]
		case arg == "--embedder":
			if i++; i >= len(args) {
				return usageError{arg + " requires a value"}
			}
			embedderArg = args[i]
		case strings.HasPrefix(arg, "--embedder="):
			embedderArg = arg[len("--embedder="):]
		case arg == "-k" || arg == "--k":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			n, err := positiveInt(args[i], "eval k")
			if err != nil {
				return err
			}
			k = n
		case strings.HasPrefix(arg, "--k="):
			n, err := positiveInt(arg[len("--k="):], "eval k")
			if err != nil {
				return err
			}
			k = n
		case arg == "--beta":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			b, err := positiveFloat(args[i], "eval beta")
			if err != nil {
				return err
			}
			beta = b
		case strings.HasPrefix(arg, "--beta="):
			b, err := positiveFloat(arg[len("--beta="):], "eval beta")
			if err != nil {
				return err
			}
			beta = b
		case arg == "--pct":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			n, err := evalPct(args[i])
			if err != nil {
				return err
			}
			pct = n
		case strings.HasPrefix(arg, "--pct="):
			n, err := evalPct(arg[len("--pct="):])
			if err != nil {
				return err
			}
			pct = n
		case arg == "--json":
			jsonOut = true
		default:
			return usageError{"unexpected eval run argument: " + arg}
		}
	}
	if questionsPath == "" {
		return usageError{"eval run requires --questions"}
	}
	embedder, err := parseEmbedderFlag(embedderArg)
	if err != nil {
		return err
	}

	f, err := os.Open(resolve(directory, questionsPath))
	if err != nil {
		return err
	}
	defer f.Close()
	qs, err := eval.LoadQuestions(f)
	if err != nil {
		return err
	}
	qs = eval.Sample(qs, pct)

	p := search.DefaultPipeline()
	var r *repo.Repository
	if corpusDir != "" {
		_, corpusRepo, cleanup, err := eval.BuildCorpusRepo(resolve(directory, corpusDir))
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := corpusRepo.Index(embedder, p); err != nil {
			return err
		}
		r = corpusRepo
	} else {
		r, err = repo.Open(directory)
		if err != nil {
			return err
		}
	}

	report, err := eval.Run(context.Background(), r, p, embedder, qs, k, beta)
	if err != nil {
		return err
	}
	if jsonOut {
		return report.WriteJSON(out)
	}
	report.WriteTable(out)
	return nil
}

// runEvalGenerate implements "eval generate --model <ollama-model> --out
// <file> [--per-file n]": it opens the repository at directory, walks its
// HEAD text blobs through eval.Generate, and writes the resulting question
// file to --out.
func runEvalGenerate(errOut io.Writer, directory string, args []string) error {
	model := ""
	outPath := ""
	perFile := defaultEvalPerFile

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--model":
			if i++; i >= len(args) {
				return usageError{arg + " requires a value"}
			}
			model = args[i]
		case strings.HasPrefix(arg, "--model="):
			model = arg[len("--model="):]
		case arg == "--out":
			if i++; i >= len(args) {
				return usageError{arg + " requires a value"}
			}
			outPath = args[i]
		case strings.HasPrefix(arg, "--out="):
			outPath = arg[len("--out="):]
		case arg == "--per-file":
			if i++; i >= len(args) {
				return usageError{arg + " requires a number"}
			}
			n, err := positiveInt(args[i], "eval generate --per-file")
			if err != nil {
				return err
			}
			perFile = n
		case strings.HasPrefix(arg, "--per-file="):
			n, err := positiveInt(arg[len("--per-file="):], "eval generate --per-file")
			if err != nil {
				return err
			}
			perFile = n
		default:
			return usageError{"unexpected eval generate argument: " + arg}
		}
	}
	if model == "" {
		return usageError{"eval generate requires --model"}
	}
	if outPath == "" {
		return usageError{"eval generate requires --out"}
	}

	r, err := repo.Open(directory)
	if err != nil {
		return err
	}

	f, err := os.Create(resolve(directory, outPath))
	if err != nil {
		return err
	}
	written, dropped, genErr := eval.Generate(context.Background(), r, evalOllamaBaseURL, model, perFile, f)
	closeErr := f.Close()
	if genErr != nil {
		return genErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Fprintf(errOut, "wrote %d question(s) to %s (%d dropped)\n", written, outPath, dropped)
	return nil
}

// positiveFloat parses value as a positive float64, or a usage error naming
// description.
func positiveFloat(value string, description string) (float64, error) {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, usageError{description + " must be a number"}
	}
	if f <= 0 {
		return 0, usageError{description + " must be positive"}
	}
	return f, nil
}

// humanBytes renders a byte count the way "du -h" does: whole bytes below
// one KiB, one decimal place at KiB and above.
func humanBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB"}
	div, exp := int64(1024), 0
	for scaled := n / 1024; scaled >= 1024 && exp < len(units)-1; scaled /= 1024 {
		div *= 1024
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}

// describe renders a failure in terms a person can act on. Path errors
// carry only the offending path, which on its own prints as an unexplained
// path with no indication of what went wrong.
func describe(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		switch {
		case errors.Is(pathErr.Err, fs.ErrPermission):
			return "permission denied: " + pathErr.Path
		case errors.Is(pathErr.Err, fs.ErrNotExist):
			return "no such file: " + pathErr.Path
		default:
			return pathErr.Err.Error() + ": " + pathErr.Path
		}
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err.Error() + ": " + linkErr.New
	}
	return err.Error()
}

// formatJavaTimestamp renders a time exactly as Java's
// DateTimeFormatter.ISO_OFFSET_DATE_TIME does: seconds always present, the
// fraction trimmed to the minimal digits needed, and "Z" for a zero offset.
func formatJavaTimestamp(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if nano := t.Nanosecond(); nano != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%09d", nano), "0")
	}
	_, offset := t.Zone()
	if offset == 0 {
		return s + "Z"
	}
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	hours, minutes, seconds := offset/3600, offset%3600/60, offset%60
	if seconds != 0 {
		return s + fmt.Sprintf("%s%02d:%02d:%02d", sign, hours, minutes, seconds)
	}
	return s + fmt.Sprintf("%s%02d:%02d", sign, hours, minutes)
}

// messageLines splits a commit message the way Java's String.lines() does:
// on any line terminator, without a trailing empty line for a final one.
func messageLines(message string) []string {
	if message == "" {
		return nil
	}
	normalized := strings.ReplaceAll(message, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	return strings.Split(normalized, "\n")
}

func firstLine(message string) string {
	lines := messageLines(message)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// printablePath keeps one change per line even for paths holding control
// characters.
func printablePath(path string) string {
	replacer := strings.NewReplacer("\\", `\\`, "\t", `\t`, "\r", `\r`, "\n", `\n`)
	return replacer.Replace(path)
}

// printableSnippet applies printablePath's same backslash escapes to a
// search snippet, plus a \xHH escape for any other Unicode control
// character. Unlike a path, snippet text is lifted straight from blob
// content, so it may carry ANSI escape sequences or other control bytes
// that must never reach the terminal unescaped. Ordinary printable text,
// including non-ASCII UTF-8, passes through unchanged.
func printableSnippet(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		default:
			if unicode.IsControl(r) {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func abbreviate(id string) string {
	return id[:12]
}

// evalPct parses value as "eval run"'s --pct: an integer in [1, 100].
// Unlike -k and --beta (positiveInt, positiveFloat), a bare strconv.Atoi
// here used to silently accept 0 or a negative number and run every
// question, exactly as pct=100 does, with no indication the flag was
// ignored; this makes --pct fail the same way its sibling flags do instead.
func evalPct(value string) (int, error) {
	n, err := positiveInt(value, "eval pct")
	if err != nil {
		return 0, err
	}
	if n > 100 {
		return 0, usageError{"eval pct must be at most 100"}
	}
	return n, nil
}

func positiveInt(value string, description string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, usageError{description + " must be a number"}
	}
	if n < 1 {
		return 0, usageError{description + " must be positive"}
	}
	return n, nil
}

func resolve(base string, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(base, value))
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "SnapVault - Git-style snapshots for any directory")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  snapvault init [directory]")
	fmt.Fprintln(out, "  snapvault [-C directory] snapshot [-m message] [--workers n]")
	fmt.Fprintln(out, "  snapvault [-C directory] log [revision] [--oneline] [--limit n]")
	fmt.Fprintln(out, "  snapvault [-C directory] diff [from [to]]")
	fmt.Fprintln(out, "  snapvault [-C directory] restore <revision> [--to directory] [--force]")
	fmt.Fprintln(out, "  snapvault [-C directory] upgrade")
	fmt.Fprintln(out, "  snapvault [-C directory] repack [--dry-run]")
	fmt.Fprintln(out, "  snapvault [-C directory] index [--embedder builtin|static|static:<name>|ollama:<model>]")
	fmt.Fprintln(out, "  snapvault [-C directory] find <query> [--limit n]")
	fmt.Fprintln(out, "  snapvault model pull <name>")
	fmt.Fprintln(out, "  snapvault model list")
	fmt.Fprintln(out, "  snapvault [-C directory] eval run --questions <file> [--corpus <dir>]")
	fmt.Fprintln(out, "      [--embedder ...] [-k n] [--beta f] [--pct n] [--json]")
	fmt.Fprintln(out, "  snapvault [-C directory] eval generate --model <ollama-model> --out <file> [--per-file n]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Revisions can be HEAD, HEAD~N, a full SHA-256 id, or a 7+ character prefix.")
	fmt.Fprintln(out, "With no revisions, diff compares HEAD to the working directory.")
	fmt.Fprintln(out, "With one revision, diff compares that snapshot to the working directory.")
}
