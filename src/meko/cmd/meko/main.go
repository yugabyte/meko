// Command meko is Meko: the CLI and the server in one binary. Run as meko-cli (a link to it), it is
// the CLI that works with what Meko keeps.
package main

import (
	"os"

	"github.com/yugabyte/meko/src/meko/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}
