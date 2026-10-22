// Package cli is the meko command: start, stop, restart, status, serve, version, token, ui, logs,
// destroy, install, uninstall, install_extension, uninstall_extension, update and list, and the MCP calls
// search, add_memory, datapacks, create_datapack, delete_datapack, upload and upload_status.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// Exit codes.
const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitNotRunning = 3 // status, when no server runs
	// exitLocked: serve found another server holding the data folder. The systemd unit tells systemd
	// not to restart on it (restarting won't help); it's EX_TEMPFAIL, as AgentK's. launchd can't skip
	// one exit code, so a serve it runs waits for the folder instead of exiting (waitForLock).
	exitLocked = 75
)

const usage = `meko runs Meko on this machine: the server, and the commands that manage it.

Usage:
  meko start     start the server in the background (through its service once installed),
                 then the extensions
  meko stop      stop the extensions, then the server; --keep-extensions leaves them running
  meko restart   restart the server alone
  meko status    show whether it's running, its port, version, children, service, updates and
                 extensions
  meko logs      show the server's and the memory engine's logs; -f follows them, -n N shows N
                 lines, --server, --memory-engine or --service shows one, --json the raw lines
  meko serve     run the server in the foreground
  meko version   show versions; --json for installers
  meko token     print the token MCP and REST clients send (Authorization: Bearer <token>);
                 --mcp prints the claude mcp add command that registers Meko with Claude Code,
                 --json the MCP URL, the token and whether a server runs
  meko ui        open the web UI in the browser, signed in with the client token;
                 --no-open only prints its link
  meko destroy   stop the server and remove the data folder, everything in it, for good;
                 it asks first, unless --force

  meko install   set this meko up: the meko command and a login service (launchd or systemd)
                 that starts it at login and restarts it on failure; --no-service skips the
                 service
  meko uninstall remove the service and the installed releases, keeping your data;
                 --purge also destroys the data and its secrets (it asks first, unless --force)
  meko update    install the newest release now (it updates itself under the service);
                 --check only looks
  meko install_extension agentk
                 install AgentK, pointed at this Meko, and start and stop it with Meko
  meko uninstall_extension agentk
                 stop starting and stopping it with Meko (it stays installed)

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
  meko list mcp-tools            the MCP tools Meko serves, each with what it does; --json for all
                                 their names, titles and summaries
                 search, add_memory, upload and upload_status take --datapack NAME|ID (default: the
                 default datapack, which Meko makes on first start and doesn't make again once
                 deleted); search takes --source auto|memory|knowledge_base|conversation and
                 --limit N; all seven take --json for the tools' raw replies

Every command takes --data-dir (default ~/.meko, or MEKO_HOME).
start, restart and serve also take --port (default 7778) and --log-level (debug, info, warn, error);
under the service, set them in config.toml instead.
`

// stdin is what add_memory, destroy and uninstall --purge read; tests replace it.
var stdin io.Reader = os.Stdin

// cmdFlags are the flags of the commands that aren't MCP calls.
type cmdFlags struct {
	keepExtensions, noService, purge, check, follow, force, noOpen bool
	lines                                                          int
	server, memoryEngine, service                                  bool
}

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
	var detached bool
	var t toolFlags
	var c cmdFlags
	switch cmd {
	case "search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload", "upload_status":
		fs.BoolVar(&t.json, "json", false, "print the tool's raw reply")
	case "version", "logs", "list":
		fs.BoolVar(&t.json, "json", false, "print raw JSON")
	case "token":
		fs.BoolVar(&t.mcp, "mcp", false, "print the claude mcp add command for Meko")
		fs.BoolVar(&t.json, "json", false, "print the MCP URL, the token and whether a server runs, as JSON")
	case "stop":
		fs.BoolVar(&c.keepExtensions, "keep-extensions", false, "leave the extensions running")
	case "install":
		fs.BoolVar(&c.noService, "no-service", false, "don't set up the login service")
	case "uninstall":
		fs.BoolVar(&c.purge, "purge", false, "also destroy the data folder and its secrets")
		fs.BoolVar(&c.force, "force", false, "with --purge, destroy without asking")
	case "destroy":
		fs.BoolVar(&c.force, "force", false, "destroy without asking")
	case "update":
		fs.BoolVar(&c.check, "check", false, "only look for an update")
	}
	if cmd == "logs" {
		fs.BoolVar(&c.follow, "f", false, "follow the logs")
		fs.BoolVar(&c.follow, "follow", false, "follow the logs")
		fs.IntVar(&c.lines, "n", 50, "how many lines to show")
		fs.BoolVar(&c.server, "server", false, "only the server's log (meko.log)")
		fs.BoolVar(&c.memoryEngine, "memory-engine", false, "only the memory engine's log (memory_engine.log)")
		fs.BoolVar(&c.service, "service", false, "only the service's stderr (service.log)")
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
	if cmd == "ui" {
		fs.BoolVar(&c.noOpen, "no-open", false, "print the link without opening the browser")
	}
	if cmd == "start" || cmd == "restart" || cmd == "serve" {
		fs.IntVar(&o.Port, "port", 0, "the port on 127.0.0.1 (default 7778)")
		fs.StringVar(&o.LogLevel, "log-level", "", "debug, info, warn or error (default info)")
	}
	if cmd == "serve" {
		// Set by start and the service: the server runs without a terminal, so it logs only to the
		// log file.
		fs.BoolVar(&detached, "detached", false, "log only to the log file")
	}
	switch cmd {
	case "start", "stop", "restart", "status", "serve", "version", "token", "ui", "logs", "destroy", "install",
		"uninstall", "install_extension", "uninstall_extension", "update", "search", "add_memory", "datapacks",
		"create_datapack", "delete_datapack", "upload", "upload_status", "list":
	default:
		fmt.Fprintf(stderr, "meko: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	// These take one argument, and upload any number; their flags may follow.
	switch cmd {
	case "search", "add_memory", "create_datapack", "delete_datapack", "upload_status", "install_extension",
		"uninstall_extension", "list":
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
	if (cmd == "install_extension" || cmd == "uninstall_extension") && t.text == "" {
		fmt.Fprintf(stderr, "meko %s: name the extension (agentk)\n", cmd)
		return exitUsage
	}
	if cmd == "version" {
		return runVersion(t.json, stdout, stderr)
	}
	if cmd == "list" {
		return runList(t.text, t.json, stdout, stderr)
	}
	root, err := paths.Resolve(*dataDir)
	if err != nil {
		return fail(stderr, err)
	}
	l := paths.New(root)
	switch cmd {
	case "stop":
		return runStop(l, c.keepExtensions, stdout, stderr)
	case "status":
		return runStatus(l, stdout, stderr)
	case "token":
		return runToken(l, t.mcp, t.json, stdout, stderr)
	case "logs":
		return runLogs(l, logsOptions{follow: c.follow, lines: c.lines, server: c.server, memoryEngine: c.memoryEngine,
			service: c.service, json: t.json}, stdout)
	case "ui":
		return runUI(l, !c.noOpen, stdout, stderr)
	case "destroy":
		return runDestroy(l, c.force, stdin, stdout, stderr)
	case "install":
		return runInstall(l, c.noService, stdout, stderr)
	case "uninstall":
		return runUninstall(l, c.purge, c.force, stdout, stderr)
	case "install_extension":
		return runInstallExtension(l, t.text, stdout, stderr)
	case "uninstall_extension":
		return runUninstallExtension(l, t.text, stdout, stderr)
	case "update":
		return runUpdate(l, c.check, stdout, stderr)
	case "search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload", "upload_status":
		return runTool(l, cmd, t, stdin, stdout, stderr)
	}
	// start loads the config too, so a bad file is reported before anything starts.
	cfg, err := config.Load(l.Config, os.Getenv, o)
	if err != nil {
		return fail(stderr, err)
	}
	switch cmd {
	case "start":
		return runStart(l, o, stdout, stderr)
	case "restart":
		return runRestart(l, o, stdout, stderr)
	}
	logOut := stderr
	if detached {
		logOut = nil
	}
	err = Serve(SignalContext(), ServeOptions{Layout: l, Config: cfg, Stderr: logOut})
	var rb *rollbackError
	var le *lockedError
	switch {
	case errors.As(err, &rb):
		// The new version failed too often: serve has put the old one back and run it in our place.
		// G204: the previous release's own binary.
		err = syscall.Exec(rb.exe, append([]string{rb.exe}, args...), os.Environ()) //nolint:gosec
		return fail(stderr, fmt.Errorf("can't run the previous version %s: %w", rb.exe, err))
	case errors.As(err, &le):
		fmt.Fprintf(stderr, "meko: %v\n", err)
		return exitLocked
	case err != nil:
		return fail(stderr, err)
	}
	return exitOK
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "meko: %v\n", err)
	return exitError
}
