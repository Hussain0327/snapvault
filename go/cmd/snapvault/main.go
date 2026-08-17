// The snapvault command brings Git-style snapshot, diff, history, and
// restore to any ordinary directory.
package main

import (
	"fmt"
	"os"

	"github.com/Hussain0327/snapvault/go/internal/cli"
)

func main() {
	workdir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, workdir))
}
