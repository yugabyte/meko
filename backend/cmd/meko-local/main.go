// Command meko-local is Meko Local: the CLI and the server in one binary.
package main

import (
	"os"

	"github.com/yugabyte/meko-local/backend/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
