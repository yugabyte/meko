// Package cli is the meko and meko-cli commands, one binary run by either name. meko runs Meko:
// install, start, stop, restart, status, logs, update, version, token, ui, ui start|stop, cloud-link,
// destroy, uninstall, extension list|install|update|delete, agent list|connect|disconnect (agent.go)
// and serve. meko-cli
// works with what it keeps, through the running server, and with the linked Meko Cloud's datapacks
// (cloudtools.go): datapacks, create_datapack, delete_datapack, export, search, add_memory, upload
// and upload_status, one command a run. help.go has each command's help.
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

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
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

// stdin is what add_memory, delete_datapack, cloud-link, destroy, uninstall --purge and extension
// delete read; tests replace it.
var stdin io.Reader = os.Stdin

// cmdFlags are the flags of the commands that aren't MCP calls.
type cmdFlags struct {
	keepExtensions, noService, noUI, purge, check, follow, force, withExtensions bool
	verbose                                                                      bool
	lines                                                                        int
	server, memoryEngine, service                                                bool
	access                                                                       string
	all                                                                          bool
	auto, version                                                                string
	extra                                                                        []string // after --
}

// Main runs the program argv0 names (meko-cli, or else meko) with args and returns the exit code.
// Run by meko-cli's old name, it runs nothing and names the meko-cli command instead.
func Main(argv0 string, args []string, stdout, stderr io.Writer) int {
	switch filepath.Base(argv0) {
	case progMekoCLI:
		return RunMekoCLI(args, stdout, stderr)
	case progMekosh:
		fmt.Fprintf(stderr, "%s is %s now: %s\n", progMekosh, progMekoCLI,
			shellQuote(append([]string{progMekoCLI}, mekoshArgs(args)...)))
		return exitUsage
	}
	return Run(args, stdout, stderr)
}

// mekoshArgs are a mekosh command line's args as meko-cli takes them. mekosh took --data-dir before
// the command too, and meko-cli takes flags only after it, so a leading --data-dir moves after the
// command. With no command, it's dropped: meko-cli alone only prints its help.
func mekoshArgs(args []string) []string {
	var lead []string
flags:
	for len(args) > 0 {
		name := strings.TrimLeft(args[0], "-")
		switch {
		case name == args[0]:
			break flags
		case name == "data-dir" && len(args) > 1:
			lead, args = append(lead, args[0], args[1]), args[2:]
		case strings.HasPrefix(name, "data-dir="):
			lead, args = append(lead, args[0]), args[1:]
		default:
			break flags
		}
	}
	if len(args) == 0 {
		return nil
	}
	return append(slices.Clone(args), lead...)
}

// RunMekoCLI runs meko-cli's command in args (without the program name) and returns the exit code.
func RunMekoCLI(args []string, stdout, stderr io.Writer) int {
	return runProg(progMekoCLI, args, stdout, stderr)
}

// Run runs meko's command in args (without the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return runProg(progMeko, args, stdout, stderr)
}

// runProg runs prog's command in args.
func runProg(prog string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		printUsage(stdout, prog, true)
		return exitOK
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		if g := rest[0]; prog == progMeko && isGroup(g) {
			if len(rest) == 1 {
				printGroupHelp(stdout, prog, g)
				return exitOK
			}
			if _, ok := lookup(prog, g+" "+rest[1]); !ok {
				return unknownSubcommand(prog, g, rest[1], stderr)
			}
			return runProg(prog, []string{g, rest[1], "--help"}, stdout, stderr)
		}
		if _, ok := lookup(prog, rest[0]); !ok {
			return unknownCommand(prog, rest[0], nil, stderr)
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
	// meko kode and meko present run an extension's own commands: all that follows is its.
	if name, ok := extensionOfVerb(cmd); ok && prog == progMeko {
		return runExtensionCommand(name, rest, stdout, stderr)
	}
	// meko extension <sub> is the command "extension <sub>", and meko agent <sub> "agent <sub>";
	// alone, each lists its subcommands.
	if isGroup(cmd) && prog == progMeko {
		if len(rest) == 0 || rest[0] == "-h" || rest[0] == "--help" || rest[0] == "help" {
			printGroupHelp(stdout, prog, cmd)
			if len(rest) == 0 {
				return exitUsage
			}
			return exitOK
		}
		if _, ok := lookup(prog, cmd+" "+rest[0]); !ok {
			return unknownSubcommand(prog, cmd, rest[0], stderr)
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	}
	// meko ui start and meko ui stop are commands of their own; meko ui alone opens it.
	if cmd == "ui" && prog == progMeko && len(rest) > 0 && (rest[0] == "start" || rest[0] == "stop") {
		cmd, rest = cmd+" "+rest[0], rest[1:]
	}
	info, ok := lookup(prog, cmd)
	if !ok || cmd == args[0] && strings.Contains(cmd, " ") { // a subcommand is two words
		return unknownCommand(prog, cmd, rest, stderr)
	}
	if cmd == "memory-extractor" {
		return runMemoryExtractor(rest, stdout, stderr)
	}
	if cmd == "answers" {
		return runAnswers(rest, stdout, stderr)
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
		fs.BoolVar(&c.noUI, "no-ui", false, "leave the web UI off (meko ui start turns it on)")
	case "uninstall":
		fs.BoolVar(&c.purge, "purge", false, "also destroy the data folder and its secrets")
		fs.BoolVar(&c.force, "force", false, "with --purge, destroy without asking")
	case "destroy":
		fs.BoolVar(&c.force, "force", false, "destroy without asking (keeping the extensions' data)")
		fs.BoolVar(&c.withExtensions, "with-extensions", false, "uninstall the extensions and remove their data too")
	case "update", "extension update":
		fs.BoolVar(&c.check, "check", false, "only look for an update")
		if cmd == "extension update" {
			fs.BoolVar(&c.all, "all", false, "every installed extension Meko updates")
			fs.StringVar(&c.auto, "auto", "", "on or off: whether Meko installs its updates on its own")
		}
	case "extension list":
		fs.BoolVar(&t.json, "json", false, "print the rows as JSON")
	case "agent list":
		fs.BoolVar(&t.json, "json", false, "print the list as JSON, as GET /api/v1/agents gives it")
	case "extension delete":
		fs.BoolVar(&c.purge, "purge", false, "also delete its data: AgentK's sessions and worktrees")
		fs.BoolVar(&c.force, "force", false, "delete without asking")
	case "extension install":
		fs.StringVar(&c.version, "version", "", "install this release rather than the newest")
		// What follows -- is the extension's own install's.
		if i := slices.Index(rest, "--"); i >= 0 {
			rest, c.extra = rest[:i], rest[i+1:]
		}
	case "cloud-link":
		fs.StringVar(&cl.url, "url", "", "the MCP server's URL (default Meko Cloud's)")
		fs.StringVar(&cl.webURL, "web-url", "", "its web app's URL (default: worked out from --url)")
		fs.StringVar(&cl.apiURL, "api-url", "", "its REST API's URL (default: the web app's plus /api/v1)")
		fs.BoolVar(&cl.off, "off", false, "unlink: the extensions use this Meko")
		fs.BoolVar(&cl.noBrowser, "no-browser", false, "signing in, print the sign-in address rather than open a browser")
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
	if cmd == "export" {
		fs.StringVar(&t.out, "o", "", "the file to write (default meko-<name>-<date>.json here)")
		fs.BoolVar(&t.force, "force", false, "replace the file if it exists")
	}
	if cmd == "start" || cmd == "restart" || cmd == "serve" {
		fs.IntVar(&o.Port, "port", 0, "Meko's port, for agents, the CLI and the web UI (default 7777)")
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
	case "search", "add_memory", "create_datapack", "delete_datapack", "upload_status", "extension install",
		"extension update", "extension delete", "export", "agent connect", "agent disconnect":
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
		fmt.Fprintln(stderr, "meko cloud-link: --off takes no URLs")
		return exitUsage
	}
	if cl.off && cl.noBrowser {
		fmt.Fprintln(stderr, "meko cloud-link: --off and --no-browser don't go together")
		return exitUsage
	}
	if cmd == "extension update" && c.all && (t.text != "" || c.auto != "") {
		fmt.Fprintln(stderr, "meko extension update: --all takes no name and no --auto")
		return exitUsage
	}
	if (cmd == "extension install" || cmd == "extension update" || cmd == "extension delete") && t.text == "" &&
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
	case "agent list":
		return runAgentList(l, t.json, stdout, stderr)
	case "agent connect":
		return runAgentConnect(l, t.text, stdout, stderr)
	case "agent disconnect":
		return runAgentDisconnect(l, t.text, stdout, stderr)
	case "logs":
		return runLogs(l, logsOptions{follow: c.follow, lines: c.lines, server: c.server, memoryEngine: c.memoryEngine,
			service: c.service, json: t.json}, stdout)
	case "ui":
		return runUI(l, stdout, stderr)
	case "ui start":
		return runUISwitch(l, true, stdout, stderr)
	case "ui stop":
		return runUISwitch(l, false, stdout, stderr)
	case "cloud-link":
		return runCloudLink(l, cl, stdin, stdout, stderr)
	case "destroy":
		return runDestroy(l, destroyOptions{force: c.force, withExtensions: c.withExtensions}, stdin, stdout, stderr)
	case "install":
		return runInstall(l, c.noService, c.access, c.noUI, stdout, stderr)
	case "uninstall":
		return runUninstall(l, c.purge, destroyOptions{force: c.force}, stdout, stderr)
	case "extension list":
		return runListExtensions(l, t.json, stdout, stderr)
	case "extension install":
		return runInstallExtension(l, t.text, extInstallOptions{version: c.version, extra: c.extra}, stdout, stderr)
	case "extension update":
		return runUpdateExtension(l, t.text, extUpdateFlags{check: c.check, all: c.all, auto: c.auto}, stdout, stderr)
	case "extension delete":
		return runDeleteExtension(l, t.text, extDeleteOptions{purge: c.purge, force: c.force}, stdin, stdout, stderr)
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
// instead (meko's usage commands moved to meko-cli), else the nearest of prog's names.
func unknownCommand(prog, cmd string, rest []string, stderr io.Writer) int {
	other := progMekoCLI
	if prog == progMekoCLI {
		other = progMeko
	}
	if c, ok := lookup(other, cmd); ok && !c.hidden {
		what := "is a meko-cli command now"
		if other == progMeko {
			what = "is a meko command"
		}
		fmt.Fprintf(stderr, "%s: %s %s: %s\n", prog, cmd, what, shellQuote(append([]string{other, cmd}, rest...)))
		return exitUsage
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n", prog, cmd)
	if n := nearest(prog, cmd); n != "" {
		// meko extensions list is meko extension list: the subcommand is kept.
		if isGroup(n) && len(rest) > 0 {
			if _, ok := lookup(prog, n+" "+rest[0]); ok {
				n += " " + rest[0]
			}
		}
		fmt.Fprintf(stderr, "Did you mean %s %s?\n", prog, n)
	}
	fmt.Fprintf(stderr, "Run %s --help to see the commands\n", prog)
	return exitUsage
}

// failAs is fail for prog: meko-cli's errors say meko-cli.
func failAs(prog string, stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "%s: %v\n", prog, err)
	return exitError
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "meko: %v\n", err)
	return exitError
}
