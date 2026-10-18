// Package cli is the meko-local command: start, stop, status, serve and version.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/yugabyte/meko-local/backend/internal/config"
	"github.com/yugabyte/meko-local/backend/internal/paths"
)

// Exit codes.
const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitNotRunning = 3 // status, when no server runs
)

const usage = `meko-local runs Meko Local, a local Meko server.

Usage:
  meko-local start     start the server in the background
  meko-local stop      stop it
  meko-local status    show whether it's running, and its port, version and children
  meko-local serve     run the server in the foreground
  meko-local version   show versions

Every command takes --data-dir (default ~/.meko-local, or MEKO_LOCAL_HOME).
start and serve also take --port (default 7778) and --log-level (debug, info, warn, error).
`

// Run runs the command in args (without the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("meko-local "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko-local, or MEKO_LOCAL_HOME)")
	var o config.Overrides
	var detached bool
	if cmd == "start" || cmd == "serve" {
		fs.IntVar(&o.Port, "port", 0, "the port on 127.0.0.1 (default 7778)")
		fs.StringVar(&o.LogLevel, "log-level", "", "debug, info, warn or error (default info)")
	}
	if cmd == "serve" {
		// Set by start: the server runs without a terminal, so it logs only to the log file.
		fs.BoolVar(&detached, "detached", false, "log only to the log file")
	}
	switch cmd {
	case "start", "stop", "status", "serve", "version":
	default:
		fmt.Fprintf(stderr, "meko-local: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "meko-local %s: unexpected argument %q\n", cmd, fs.Arg(0))
		return exitUsage
	}
	if cmd == "version" {
		return runVersion(stdout, stderr)
	}
	root, err := paths.Resolve(*dataDir)
	if err != nil {
		return fail(stderr, err)
	}
	l := paths.New(root)
	switch cmd {
	case "stop":
		return runStop(l, stdout, stderr)
	case "status":
		return runStatus(l, stdout, stderr)
	}
	// start loads the config too, so a bad file is reported before anything starts.
	cfg, err := config.Load(l.Config, os.Getenv, o)
	if err != nil {
		return fail(stderr, err)
	}
	if cmd == "start" {
		return runStart(l, o, stdout, stderr)
	}
	logOut := stderr
	if detached {
		logOut = nil
	}
	if err := Serve(SignalContext(), ServeOptions{Layout: l, Config: cfg, Stderr: logOut}); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "meko-local: %v\n", err)
	return exitError
}
