package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPCheckpointsAtStartAndServesTools(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "store")

	old := mcpInput
	t.Cleanup(func() { mcpInput = old })
	mcpInput = strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_checkpoints","arguments":{}}}`,
	}, "\n") + "\n")

	var out, errOut bytes.Buffer
	if code := Run([]string{"-C", work, "mcp", "--store", store}, &out, &errOut, base); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want exactly two protocol responses and nothing else", out.String())
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) {
		t.Errorf("initialize reply = %s", lines[0])
	}
	if !strings.Contains(lines[1], "auto: session start") {
		t.Errorf("list_checkpoints reply = %s, want the startup checkpoint", lines[1])
	}
	if _, err := os.Lstat(filepath.Join(work, ".snapvault")); !os.IsNotExist(err) {
		t.Errorf("mcp created .snapvault inside the protected folder (err = %v)", err)
	}
}

func TestMCPRejectsBadOptions(t *testing.T) {
	for _, args := range [][]string{{"mcp", "--interval", "10ms"}, {"mcp", "--bogus"}, {"mcp", "--store"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut, t.TempDir()); code != 2 {
			t.Errorf("%v: exit %d, want 2 (usage error)", args, code)
		}
	}
}

func TestGlobalStoreRunsCommandsAgainstACheckpointStore(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "store")
	old := mcpInput
	t.Cleanup(func() { mcpInput = old })
	mcpInput = strings.NewReader("")
	var out, errOut bytes.Buffer
	if code := Run([]string{"-C", work, "mcp", "--store", store}, &out, &errOut, base); code != 0 {
		t.Fatalf("mcp exit %d: %s", code, errOut.String())
	}

	out.Reset()
	if code := Run([]string{"--store", store, "log", "--oneline"}, &out, &errOut, base); code != 0 {
		t.Fatalf("log exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "auto: session start") {
		t.Errorf("log = %q, want the session-start checkpoint", out.String())
	}

	if err := os.Remove(filepath.Join(work, "a.txt")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"--store", store, "diff"}, &out, &errOut, base); code != 0 || !strings.Contains(out.String(), "a.txt") {
		t.Errorf("diff exit %d, output %q; want a.txt reported", code, out.String())
	}

	errOut.Reset()
	if code := Run([]string{"--store", store, "find", "x"}, &out, &errOut, base); code != 2 {
		t.Errorf("find with --store exit %d, want 2", code)
	}
	if code := Run([]string{"--store", filepath.Join(base, "nope"), "log"}, &out, &errOut, base); code != 1 {
		t.Errorf("log on a missing store exit %d, want 1", code)
	}
}

func TestMCPIgnoreKeepsNamedDirectoriesOutOfCheckpoints(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	for _, rel := range []string{"a.txt", "node_modules/dep.js"} {
		path := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(base, "store")
	old := mcpInput
	t.Cleanup(func() { mcpInput = old })
	mcpInput = strings.NewReader("")
	var out, errOut bytes.Buffer
	if code := Run([]string{"-C", work, "mcp", "--store", store, "--ignore", "node_modules"}, &out, &errOut, base); code != 0 {
		t.Fatalf("mcp exit %d: %s", code, errOut.String())
	}
	if err := os.WriteFile(filepath.Join(work, "node_modules", "dep.js"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"--store", store, "diff"}, &out, &errOut, base); code != 0 {
		t.Fatalf("diff exit %d: %s", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "No changes." {
		t.Errorf("diff = %q; a change under an ignored directory must not show up", got)
	}
}
