// Package cli implements the SnapVault command-line interface with the same
// commands, output, and exit codes as the Java reference implementation,
// plus a --workers flag bounding the concurrent hashing pool.
package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/object"
	"github.com/Hussain0327/snapvault/go/internal/repo"
)

const defaultLogLimit = 50

// usageError reports a mistake in how the command was invoked; it exits
// with status 2 and a pointer at the help text.
type usageError struct {
	message string
}

func (e usageError) Error() string { return e.message }

// Run executes one CLI invocation and returns its exit code.
func Run(args []string, out, errOut io.Writer, workdir string) int {
	err := execute(args, out, workdir)
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

func execute(args []string, out io.Writer, workdir string) error {
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

func abbreviate(id string) string {
	return id[:12]
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
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Revisions can be HEAD, HEAD~N, a full SHA-256 id, or a 7+ character prefix.")
	fmt.Fprintln(out, "With no revisions, diff compares HEAD to the working directory.")
	fmt.Fprintln(out, "With one revision, diff compares that snapshot to the working directory.")
}
