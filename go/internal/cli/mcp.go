package cli

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/Hussain0327/snapvault/go/internal/guard"
	"github.com/Hussain0327/snapvault/go/internal/mcp"
	"github.com/Hussain0327/snapvault/go/internal/repo"
)

// defaultMCPInterval is how often "snapvault mcp" checkpoints the folder
// when --interval is not given.
const defaultMCPInterval = 5 * time.Second

// mcpServerVersion is the version "snapvault mcp" reports in its
// initialize reply; it matches "snapvault version".
const mcpServerVersion = "1.0.0"

// mcpInput is where "snapvault mcp" reads protocol messages; a test seam.
var mcpInput io.Reader = os.Stdin

// runMCP serves the agent-undo tools for directory over stdio until the
// client closes standard input. Each --ignore names a file or directory
// (such as node_modules) to leave out of checkpoints at any depth. It checkpoints the folder once at startup
// and then automatically for as long as the session lasts.
func runMCP(out io.Writer, loc location, args []string) error {
	directory := loc.directory
	storeDir := loc.store
	interval := defaultMCPInterval
	var ignore []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ignore":
			if i+1 >= len(args) {
				return usageError{"--ignore requires a file or directory name"}
			}
			ignore = append(ignore, args[i+1])
			i++
		case "--store":
			if i+1 >= len(args) {
				return usageError{"--store requires a directory"}
			}
			storeDir = resolve(directory, args[i+1])
			i++
		case "--interval":
			if i+1 >= len(args) {
				return usageError{"--interval requires a duration"}
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < time.Second {
				return usageError{"--interval must be a duration of at least 1s, such as 5s"}
			}
			interval = d
			i++
		default:
			return usageError{"unknown mcp option: " + args[i]}
		}
	}
	if storeDir == "" {
		d, err := guard.DefaultStoreDir(directory)
		if err != nil {
			return err
		}
		storeDir = d
	}

	r, err := repo.OpenDetached(directory, storeDir)
	if err != nil {
		return err
	}
	// Without --ignore the store keeps whatever rules it already records.
	if len(ignore) > 0 {
		if err := r.SetIgnore(ignore); err != nil {
			return err
		}
	}
	session := guard.New(r, interval)
	if _, err := session.Checkpoint("session start"); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Run(ctx)

	server := &mcp.Server{
		Name:         "snapvault",
		Version:      mcpServerVersion,
		Instructions: guard.Instructions,
		Tools:        session.Tools(),
	}
	return server.Serve(ctx, mcpInput, out)
}
