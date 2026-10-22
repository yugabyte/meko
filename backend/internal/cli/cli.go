// Package cli is the meko command: start, stop, status, serve, version, token, destroy, and the MCP
// calls search, add_memory, datapacks, create_datapack, delete_datapack, upload and upload_status.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// Exit codes.
const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitNotRunning = 3 // status, when no server runs
)

const usage = `meko runs Meko on this machine: the server, and the commands that manage it.

Usage:
  meko start     start the server in the background
  meko stop      stop it
  meko status    show whether it's running, and its port, version and children
  meko serve     run the server in the foreground
  meko version   show versions
  meko token     print the token MCP and REST clients send (Authorization: Bearer <token>);
                 --mcp prints the claude mcp add command that registers Meko with Claude Code
  meko destroy   stop the server and remove the data folder, everything in it, for good;
                 it asks first, unless --force

  meko search "<question>"       search memories, the knowledge base and past conversations
                                 (context_search)
  meko add_memory "<text>"       save a memory, word for word (memory_add); - or no text reads stdin
  meko datapacks                 list the datapacks
  meko create_datapack <name>    create a datapack (datapack_create)
  meko delete_datapack <name|id>
                                 delete a datapack and everything in it, for good (datapack_delete);
                                 it asks first, unless --force
  meko upload <file>...          add files to the knowledge base (knowledgebase_upload_document) and
                                 wait until they're indexed; --no-wait returns once they're uploaded
  meko upload_status <key>       how an uploaded file is doing (knowledgebase_document_status)
                 search, add_memory, upload and upload_status take --datapack NAME|ID (default: the
                 default datapack, which Meko makes on first start and doesn't make again once
                 deleted); search takes --source auto|memory|knowledge_base|conversation and
                 --limit N; all seven take --json for the tools' raw replies

Every command takes --data-dir (default ~/.meko, or MEKO_HOME).
start and serve also take --port (default 7778) and --log-level (debug, info, warn, error).
`

// stdin is what add_memory and destroy read; tests replace it.
var stdin io.Reader = os.Stdin

// Run runs the command in args (without the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("meko "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	var o config.Overrides
	var detached, force bool
	var t toolFlags
	switch cmd {
	case "search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload", "upload_status":
		fs.BoolVar(&t.json, "json", false, "print the tool's raw reply")
	case "token":
		fs.BoolVar(&t.mcp, "mcp", false, "print the claude mcp add command for Meko")
	}
	if cmd == "search" || cmd == "add_memory" || cmd == "upload" || cmd == "upload_status" {
		fs.StringVar(&t.datapack, "datapack", "", "the datapack, by name or id")
	}
	if cmd == "upload" {
		fs.BoolVar(&t.noWait, "no-wait", false, "return once the files are uploaded, before they're indexed")
	}
	if cmd == "delete_datapack" {
		fs.BoolVar(&t.force, "force", false, "delete without asking")
	}
	if cmd == "search" {
		fs.StringVar(&t.source, "source", "auto", "auto, memory, knowledge_base or conversation")
		fs.IntVar(&t.limit, "limit", 8, "the most results per source")
	}
	if cmd == "destroy" {
		fs.BoolVar(&force, "force", false, "destroy without asking")
	}
	if cmd == "start" || cmd == "serve" {
		fs.IntVar(&o.Port, "port", 0, "the port on 127.0.0.1 (default 7778)")
		fs.StringVar(&o.LogLevel, "log-level", "", "debug, info, warn or error (default info)")
	}
	if cmd == "serve" {
		// Set by start: the server runs without a terminal, so it logs only to the log file.
		fs.BoolVar(&detached, "detached", false, "log only to the log file")
	}
	switch cmd {
	case "start", "stop", "status", "serve", "version", "token", "destroy", "search", "add_memory",
		"datapacks", "create_datapack", "delete_datapack", "upload", "upload_status":
	default:
		fmt.Fprintf(stderr, "meko: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	// These take one argument, and upload any number; their flags may follow.
	switch cmd {
	case "search", "add_memory", "create_datapack", "delete_datapack", "upload_status":
		if fs.NArg() > 0 {
			t.text = fs.Arg(0)
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return exitUsage
			}
		}
	case "upload":
		for fs.NArg() > 0 {
			t.files = append(t.files, fs.Arg(0))
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return exitUsage
			}
		}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "meko %s: unexpected argument %q\n", cmd, fs.Arg(0))
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
	case "token":
		return runToken(l, t.mcp, stdout, stderr)
	case "destroy":
		return runDestroy(l, force, stdin, stdout, stderr)
	case "search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload", "upload_status":
		return runTool(l, cmd, t, stdin, stdout, stderr)
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
	fmt.Fprintf(stderr, "meko: %v\n", err)
	return exitError
}
