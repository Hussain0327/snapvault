package guard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/mcp"
	"github.com/Hussain0327/snapvault/go/internal/repo"
)

func newSession(t *testing.T) (*Session, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "work")
	writeFile(t, root, "notes.txt", "original\n")
	r, err := repo.OpenDetached(root, filepath.Join(base, "store"))
	if err != nil {
		t.Fatalf("OpenDetached = %v", err)
	}
	return New(r, time.Hour), r.Root()
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, s *Session, name string, args string) (string, error) {
	t.Helper()
	for _, tool := range s.Tools() {
		if tool.Name == name {
			return tool.Handler(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool named %s", name)
	return "", nil
}

func TestCheckpointIsSkippedWhenNothingChanged(t *testing.T) {
	s, root := newSession(t)
	if _, err := s.Checkpoint("session start"); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, s, "checkpoint", `{"message":"before refactor"}`)
	if err != nil || !strings.Contains(out, "No changes") {
		t.Errorf("checkpoint = %q, %v; want a no-changes report", out, err)
	}
	writeFile(t, root, "notes.txt", "edited\n")
	out, err = call(t, s, "checkpoint", `{"message":"before refactor"}`)
	if err != nil || !strings.Contains(out, "Created checkpoint") {
		t.Errorf("checkpoint = %q, %v; want a new checkpoint", out, err)
	}
	listed, err := call(t, s, "list_checkpoints", `{}`)
	if err != nil || !strings.Contains(listed, "agent: before refactor") || !strings.Contains(listed, "auto: session start") {
		t.Errorf("list_checkpoints = %q, %v", listed, err)
	}
}

func TestRestoreUndoesAnAgentDeleteAndCanItselfBeUndone(t *testing.T) {
	s, root := newSession(t)
	first, err := s.Checkpoint("session start")
	if err != nil {
		t.Fatal(err)
	}
	// The agent deletes the file with a shell command, bypassing any tool.
	if err := os.Remove(filepath.Join(root, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "scratch.txt", "agent output\n")

	diff, err := call(t, s, "diff", `{"from":"`+first+`"}`)
	if err != nil || !strings.Contains(diff, "D notes.txt") || !strings.Contains(diff, "A scratch.txt") {
		t.Fatalf("diff = %q, %v", diff, err)
	}

	out, err := call(t, s, "restore", `{"checkpoint":"`+first+`","paths":["notes.txt"]}`)
	if err != nil {
		t.Fatalf("restore = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "notes.txt")); string(data) != "original\n" {
		t.Errorf("notes.txt = %q after restore", data)
	}
	if _, err := os.Stat(filepath.Join(root, "scratch.txt")); err != nil {
		t.Errorf("scratch.txt was touched by a path-scoped restore: %v", err)
	}
	undo := undoCheckpoint(t, out)

	// Undo the undo: the pre-restore checkpoint still has scratch.txt and no notes.txt.
	if _, err := call(t, s, "restore", `{"checkpoint":"`+undo+`"}`); err != nil {
		t.Fatalf("whole-folder restore = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("notes.txt exists after undoing the restore (err = %v)", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "scratch.txt")); string(data) != "agent output\n" {
		t.Errorf("scratch.txt = %q after undoing the restore", data)
	}
}

func undoCheckpoint(t *testing.T, restoreOutput string) string {
	t.Helper()
	const marker = "restore checkpoint "
	i := strings.Index(restoreOutput, marker)
	if i < 0 {
		t.Fatalf("restore output %q does not name the checkpoint that undoes it", restoreOutput)
	}
	return strings.TrimSuffix(strings.Fields(restoreOutput[i+len(marker):])[0], ".")
}

func TestNextWaitBacksOffForSlowScans(t *testing.T) {
	if got := nextWait(5*time.Second, 10*time.Millisecond); got != 5*time.Second {
		t.Errorf("fast scan wait = %v, want the interval", got)
	}
	if got := nextWait(5*time.Second, 2*time.Second); got != 20*time.Second {
		t.Errorf("slow scan wait = %v, want 10x the scan time", got)
	}
}

func TestDefaultStoreDirIsOutsideTheFolderAndStable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	a, err := DefaultStoreDir(root)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := DefaultStoreDir(root)
	other, _ := DefaultStoreDir(t.TempDir())
	if a != b || a == other || strings.HasPrefix(a, root) {
		t.Errorf("DefaultStoreDir = %q, %q, %q", a, b, other)
	}
}

func TestSessionServesItsToolsOverMCP(t *testing.T) {
	s, _ := newSession(t)
	server := &mcp.Server{Name: "snapvault", Version: "test", Tools: s.Tools()}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out strings.Builder
	if err := server.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"checkpoint", "list_checkpoints", "diff", "restore"} {
		if !strings.Contains(out.String(), `"name":"`+name+`"`) {
			t.Errorf("tools/list is missing %s: %s", name, out.String())
		}
	}
}
