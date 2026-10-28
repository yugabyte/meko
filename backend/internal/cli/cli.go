// Package cli is the meko and mekosh commands, one binary run by either name. meko runs Meko:
// install, start, stop, restart, status, logs, update, version, connect (connect.go), token, ui,
// setup_link, cloud_link, destroy, uninstall, install_extension, update_extension, uninstall_extension
// and serve. mekosh works with what it keeps, through the running server, and with the linked Meko
// Cloud's datapacks (cloudtools.go): datapacks, create_datapack, delete_datapack, export, search,
// add_memory, upload and upload_status, one at a time or at its prompt (shell.go). help.go has each
// command's help.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
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

// stdin is what add_memory, destroy and uninstall --purge read; tests replace it.
var stdin io.Reader = os.Stdin

// cmdFlags are the flags of the commands that aren't MCP calls.
type cmdFlags struct {
	keepExtensions, noService, purge, check, follow, force, noOpen, withExtensions bool
	verbose                                                                        bool
	lines                                                                          int
	server, memoryEngine, service                                                  bool
	access                                                                         string
	all                                                                            bool
	auto, version                                                                  string
	extra                                                                          []string // after --
}

// Main runs the program argv0 names (mekosh, or else meko) with args and returns the exit code.
func Main(argv0 string, args []string, stdout, stderr io.Writer) int {
	if filepath.Base(argv0) == progMekosh {
		return RunMekosh(args, stdout, stderr)
	}
	return Run(args, stdout, stderr)
}

// Run runs meko's command in args (without the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return runProg(progMeko, args, stdout, stderr)
}

// runProg runs prog's command in args.
func runProg(prog string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout, prog)
		return exitOK
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		if len(rest) == 0 {
			printUsage(stdout, prog)
			return exitOK
		}
		c, ok := lookup(prog, rest[0])
		if !ok {
			return unknownCommand(prog, rest[0], nil, stderr)
		}
		if c.promptOnly {
			printCommandHelp(stdout, prog, c, nil)
			return exitOK
		}
		return runProg(prog, []string{rest[0], "--help"}, stdout, stderr)
	case "--version":
		if prog == progMeko {
			cmd = "version"
		}
	}
	// What an extension's launcher runs; not in the help, not for people.
	if cmd == "_launch" && prog == progMeko {
		return runLaunch(rest, stderr)
	}
	info, ok := lookup(prog, cmd)
	if !ok {
		return unknownCommand(prog, cmd, rest, stderr)
	}
	if info.promptOnly {
		fmt.Fprintf(stderr, "%s: %s works only at the prompt (run mekosh alone)\n", prog, cmd)
		return exitUsage
	}
	if cmd == "memory-extractor" {
		return runMemoryExtractor(rest, stdout, stderr)
	}
	fs := flag.NewFlagSet(prog+" "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // -h prints the command's help, below; a bad flag says where to find it
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	var o config.Overrides
	var detached bool
	var t toolFlags
	var c cmdFlags
	var cl cloudFlags
	switch cmd {
	case "search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload", "upload_status":
		fs.BoolVar(&t.json, "json", false, "print the tool's raw reply")
	case "version", "logs":
		fs.BoolVar(&t.json, "json", false, "print raw JSON")
	case "status":
		fs.BoolVar(&c.verbose, "verbose", false, "everything: the models, certificate, service, updates and links")
		fs.BoolVar(&t.json, "json", false, "print the server's status reply")
	case "token":
		fs.BoolVar(&t.json, "json", false,
			"print the MCP URL, the token, whether a server runs and the Meko Cloud link, as JSON")
	case "stop":
		fs.BoolVar(&c.keepExtensions, "keep-extensions", false, "leave the extensions running")
	case "install":
		fs.BoolVar(&c.noService, "no-service", false, "don't set up the login service")
		fs.StringVar(&c.access, "access", "", "how browsers reach the web UI: local or remote")
	case "uninstall":
		fs.BoolVar(&c.purge, "purge", false, "also destroy the data folder and its secrets")
		fs.BoolVar(&c.force, "force", false, "with --purge, destroy without asking (keeping the extensions' data)")
		fs.BoolVar(&c.withExtensions, "with-extensions", false,
			"with --purge, uninstall the extensions and remove their data too")
	case "destroy":
		fs.BoolVar(&c.force, "force", false, "destroy without asking (keeping the extensions' data)")
		fs.BoolVar(&c.withExtensions, "with-extensions", false, "uninstall the extensions and remove their data too")
	case "update", "update_extension":
		fs.BoolVar(&c.check, "check", false, "only look for an update")
		if cmd == "update_extension" {
			fs.BoolVar(&c.all, "all", false, "every installed extension Meko updates")
			fs.StringVar(&c.auto, "auto", "", "on or off: whether Meko installs its updates on its own")
		}
	case "install_extension":
		fs.StringVar(&c.version, "version", "", "install this release rather than the newest")
		// What follows -- is the extension's own install's.
		if i := slices.Index(rest, "--"); i >= 0 {
			rest, c.extra = rest[:i], rest[i+1:]
		}
	case "cloud_link":
		fs.StringVar(&cl.url, "url", "", "the MCP server's URL (default Meko Cloud's)")
		fs.StringVar(&cl.webURL, "web-url", "", "its web app's URL (default: worked out from --url)")
		fs.StringVar(&cl.apiURL, "api-url", "", "its REST API's URL (default: the web app's plus /api/v1)")
		fs.BoolVar(&cl.off, "off", false, "unlink: the extensions use this Meko")
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
		fs.StringVar(&t.datapack, "datapack", "", "the datapack, by name or id (cloud: or local: before it picks the side)")
	}
	if cmd == "upload" {
		fs.BoolVar(&t.noWait, "no-wait", false, "return once the files are uploaded, before they're indexed")
	}
	if cmd == "delete_datapack" {
		fs.BoolVar(&t.force, "force", false, "delete without asking")
	}
	if cmd == "create_datapack" {
		fs.BoolVar(&t.cloud, "cloud", false, "create it in the linked Meko Cloud")
	}
	if cmd == "datapacks" {
		fs.BoolVar(&t.local, "local", false, "only this Meko's datapacks, not the linked Meko Cloud's")
	}
	if cmd == "search" {
		fs.StringVar(&t.source, "source", "auto", "auto, memory, knowledge_base or conversation")
		fs.IntVar(&t.limit, "limit", 8, "the most results per source")
	}
	if cmd == "export" {
		fs.StringVar(&t.out, "o", "", "the file to write (default meko-<name>-<date>.json here)")
		fs.BoolVar(&t.force, "force", false, "replace the file if it exists")
	}
	if cmd == "ui" {
		fs.BoolVar(&c.noOpen, "no-open", false, "print the link without opening the browser")
	}
	if cmd == "start" || cmd == "restart" || cmd == "serve" {
		fs.IntVar(&o.Port, "port", 0, "the agents' listener's port (MCP and API) on 127.0.0.1 (default 7778)")
		fs.StringVar(&o.LogLevel, "log-level", "", "debug, info, warn or error (default info)")
	}
	if cmd == "serve" {
		// Set by start and the service: the server runs without a terminal, so it logs only to the
		// log file.
		fs.BoolVar(&detached, "detached", false, "log only to the log file")
	}
	// parse parses flags, and says how a bad one went: -h prints the command's help (exit 0).
	parse := func(args []string) (int, bool) {
		err := fs.Parse(args)
		switch {
		case errors.Is(err, flag.ErrHelp):
			printCommandHelp(stdout, prog, info, fs)
			return exitOK, false
		case err != nil:
			fmt.Fprintf(stderr, "Run %s %s --help for its flags\n", prog, cmd)
			return exitUsage, false
		}
		return 0, true
	}
	if code, ok := parse(rest); !ok {
		return code
	}
	// These take one argument, and upload any number; their flags may follow.
	switch cmd {
	case "search", "add_memory", "create_datapack", "delete_datapack", "upload_status", "install_extension",
		"update_extension", "uninstall_extension", "export", "ui", "connect":
		if fs.NArg() > 0 {
			t.text = fs.Arg(0)
			if code, ok := parse(fs.Args()[1:]); !ok {
				return code
			}
		}
	case "upload":
		for fs.NArg() > 0 {
			t.files = append(t.files, fs.Arg(0))
			if code, ok := parse(fs.Args()[1:]); !ok {
				return code
			}
		}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s %s: unexpected argument %q\n", prog, cmd, fs.Arg(0))
		return exitUsage
	}
	cl.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { cl.set[fl.Name] = true })
	if cl.off && (cl.set["url"] || cl.set["web-url"] || cl.set["api-url"]) {
		fmt.Fprintln(stderr, "meko cloud_link: --off takes no URLs")
		return exitUsage
	}
	if cmd == "update_extension" && c.all && (t.text != "" || c.auto != "") {
		fmt.Fprintln(stderr, "meko update_extension: --all takes no name and no --auto")
		return exitUsage
	}
	if (cmd == "install_extension" || cmd == "update_extension" || cmd == "uninstall_extension") && t.text == "" &&
		!c.all {
		fmt.Fprintf(stderr, "meko %s: name the extension (%s)\n", cmd, strings.Join(extensions.KnownNames(), ", "))
		return exitUsage
	}
	if cmd == "version" {
		return runVersion(t.json, stdout, stderr)
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
		return runStatus(l, statusOptions{verbose: c.verbose, json: t.json}, stdout, stderr)
	case "token":
		return runToken(l, t.json, stdout, stderr)
	case "connect":
		return runConnect(l, t.text, stdout, stderr)
	case "logs":
		return runLogs(l, logsOptions{follow: c.follow, lines: c.lines, server: c.server, memoryEngine: c.memoryEngine,
			service: c.service, json: t.json}, stdout)
	case "ui":
		return runUI(l, t.text, !c.noOpen, stdout, stderr)
	case "setup_link":
		return runSetupLink(l, stdout, stderr)
	case "cloud_link":
		return runCloudLink(l, cl, stdin, stdout, stderr)
	case "destroy":
		return runDestroy(l, destroyOptions{force: c.force, withExtensions: c.withExtensions}, stdin, stdout, stderr)
	case "install":
		return runInstall(l, c.noService, c.access, stdout, stderr)
	case "uninstall":
		return runUninstall(l, c.purge, destroyOptions{force: c.force, withExtensions: c.withExtensions}, stdout, stderr)
	case "install_extension":
		return runInstallExtension(l, t.text, extInstallOptions{version: c.version, extra: c.extra}, stdout, stderr)
	case "update_extension":
		return runUpdateExtension(l, t.text, extUpdateFlags{check: c.check, all: c.all, auto: c.auto}, stdout, stderr)
	case "uninstall_extension":
		return runUninstallExtension(l, t.text, stdout, stderr)
	case "update":
		return runUpdate(l, c.check, stdout, stderr)
	case "search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload", "upload_status", "export":
		return runTool(l, prog, cmd, t, stdin, stdout, stderr)
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
	var ex *execError
	var le *lockedError
	switch {
	case errors.As(err, &ex):
		// Serve has stopped for another version to run in our place: the previous one, put back after
		// the new one failed too often, or the new one, with no service to start it.
		// G204: a release's own binary.
		err = syscall.Exec(ex.exe, append([]string{ex.exe}, args...), os.Environ()) //nolint:gosec
		return fail(stderr, fmt.Errorf("can't run %s (%s): %w", ex.exe, ex.why, err))
	case errors.As(err, &le):
		fmt.Fprintf(stderr, "meko: %v\n", err)
		return exitLocked
	case err != nil:
		return fail(stderr, err)
	}
	return exitOK
}

// unknownCommand says cmd isn't one of prog's: when it's the other program's, which one to run
// instead (meko's usage commands moved to mekosh), else the nearest of prog's names.
func unknownCommand(prog, cmd string, rest []string, stderr io.Writer) int {
	other := progMekosh
	if prog == progMekosh {
		other = progMeko
	}
	if c, ok := lookup(other, cmd); ok && !c.hidden && !c.promptOnly {
		what := "is a mekosh command now"
		if other == progMeko {
			what = "is a meko command"
		}
		fmt.Fprintf(stderr, "%s: %s %s: %s\n", prog, cmd, what, shellQuote(append([]string{other, cmd}, rest...)))
		return exitUsage
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n", prog, cmd)
	if n := nearest(prog, cmd); n != "" {
		fmt.Fprintf(stderr, "Did you mean %s %s?\n", prog, n)
	}
	fmt.Fprintf(stderr, "Run %s --help to see the commands\n", prog)
	return exitUsage
}

// failAs is fail for prog: mekosh's errors say mekosh.
func failAs(prog string, stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "%s: %v\n", prog, err)
	return exitError
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "meko: %v\n", err)
	return exitError
}
