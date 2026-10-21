// Command meko is Meko: the CLI and the server in one binary.
package main

import (
	"os"

	"github.com/yugabyte/meko/backend/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
