// Command meko-local is Meko Local: the CLI and the server in one binary. For now it only
// reports its version; the commands come with the foundation.
package main

import (
	"fmt"
	"os"

	"github.com/yugabyte/meko-local/backend/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Printf("meko-local %s (commit %s)\n", version.Version, version.Commit)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: meko-local version")
	os.Exit(2)
}
