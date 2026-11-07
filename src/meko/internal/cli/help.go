package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/spin"
)

// The two programs this binary is: meko runs Meko, meko-cli works with what it keeps. cmd/meko picks
// one from the name it was run by.
const (
	progMeko    = "meko"
	progMekoCLI = "meko-cli"
	// progMekosh is meko-cli's old name. Run by it, the binary names meko-cli instead, which is
	// why an install's ~/.local/bin/mekosh link stays until meko uninstall removes it.
	progMekosh = "mekosh"
)

// docsURL is where the CLI's docs live; each command's help links to its anchor there.
const docsURL = "https://mekodata.ai/docs/cli"

// command is one command's help: the group meko --help (or meko-cli --help) lists it under, a one-line
// summary, its usage line and examples. Its flags come from its flag set. A hidden command runs but
// isn't listed. A command of two words (meko extension list) is a subcommand of the first, whose own
// row (subcommands, with no group) isn't listed: its help lists its subcommands.
type command struct {
	name, group, summary, usage string
	examples                    []example
	hidden, subcommands         bool
}

type example struct{ cmd, note string }

// groupExtension is the command whose subcommands work on extensions: meko extension list|install|
// update|delete.
const groupExtension = "extension"

// isGroup says whether name is a command of subcommands (meko extension, meko agent).
func isGroup(name string) bool { return name == groupExtension || name == groupAgent }

var mekoGroups = []string{"Get started", "Run", "Connect", "Extensions", "Remove"}

var mekoCommands = []command{
	{name: "install", group: "Get started", summary: "Set Meko up and start it at login",
		usage: "meko install [flags]", examples: []example{
			{"meko install", "set Meko up with a login service (launchd or systemd), and start it"},
			{"meko install --access remote", "let browsers on other computers reach the web UI, over HTTPS, with no login"},
			{"meko install --no-ui", "set Meko up with the web UI off (meko ui start turns it on)"},
			{"meko install --no-service", "set it up without a service; start it with meko start"},
		}},
	{name: "start", group: "Get started", summary: "Start Meko and its extensions",
		usage: "meko start [flags]", examples: []example{
			{"meko start", "start Meko (through its service once installed), then the extensions"},
			{"meko start --port 7790", "run the agents' listener on another port (without a service)"},
		}},
	{name: "status", group: "Get started", summary: "Show whether Meko is running, and its links",
		usage: "meko status [flags]", examples: []example{
			{"meko status", "whether Meko is running, its links and its extensions"},
			{"meko status --verbose", "everything: the models, certificate, service, updates and Meko Cloud link"},
			{"meko status --json", "the server's status reply, for scripts and agents"},
		}},
	{name: "ui", group: "Get started", summary: "Open the web UI in your browser",
		usage: "meko ui", examples: []example{
			{"meko ui", "open Meko's web UI (with no browser here, such as over SSH, print its link)"},
			{"meko ui stop", "turn the web UI off, and meko ui start to turn it on again"},
		}},
	{name: "ui start", group: "Run", summary: "Turn the web UI on, and keep it on",
		usage: "meko ui start", examples: []example{
			{"meko ui start", "serve the web UI again after meko ui stop or meko install --no-ui"},
		}},
	{name: "ui stop", group: "Run", summary: "Turn the web UI off, and keep it off",
		usage: "meko ui stop", examples: []example{
			{"meko ui stop", "close the web UI's port, AgentK's page with it, until meko ui start"},
		}},
	{name: "stop", group: "Run", summary: "Stop Meko and its extensions",
		usage: "meko stop [flags]", examples: []example{
			{"meko stop", "stop the extensions, then Meko"},
			{"meko stop --keep-extensions", "stop Meko alone"},
		}},
	{name: "restart", group: "Run", summary: "Restart Meko (the extensions keep running)",
		usage: "meko restart [flags]", examples: []example{
			{"meko restart", "restart Meko; the extensions keep running"},
		}},
	{name: "logs", group: "Run", summary: "Show Meko's logs",
		usage: "meko logs [flags]", examples: []example{
			{"meko logs", "the last 50 lines of the server's and the context engine's logs"},
			{"meko logs -f", "follow them"},
			{"meko logs --context-engine -n 200", "the context engine's last 200 lines"},
		}},
	{name: "update", group: "Run", summary: "Install Meko's newest release, then the extensions'",
		usage: "meko update [flags]", examples: []example{
			{"meko update", "install Meko's newest release now (it updates itself under the service), then every extension's"},
			{"meko update --check", "only look"},
		}},
	{name: "version", group: "Run", summary: "Show Meko's version (also --version)",
		usage: "meko version [flags]", examples: []example{
			{"meko version", "Meko's version and its parts"},
			{"meko version --json", "the same, for installers"},
		}},
	{name: "memory-extractor", group: "Run", summary: "Extract memories from conversations through your own Claude Code",
		usage: "meko memory-extractor use|off|status|run|datapack [args] [flags]", examples: []example{
			{"meko memory-extractor use claude", "turn it on: every 5 minutes, waiting turns go to claude -p (no tools)"},
			{"meko memory-extractor use claude --every 10m --model sonnet", "run less often, with another model"},
			{"meko memory-extractor status", "the tool, the interval, what waits, the last run and its error"},
			{"meko memory-extractor run", "extract the waiting turns now"},
			{"meko memory-extractor datapack work --profile coding", "rules for coding conversations in that datapack"},
			{"meko memory-extractor datapack personal --off", "don't extract that datapack's conversations"},
			{"meko memory-extractor off", "turn it off"},
		}},
	{name: "answers", group: "Run", summary: "Answer from Search's results in the web UI, through your own Claude Code",
		usage: "meko answers use|off|status [args] [flags]", examples: []example{
			{"meko answers use claude", "turn them on: Search sends its results to claude -p (no tools) for an answer"},
			{"meko answers use claude --model sonnet", "with another model"},
			{"meko answers status", "whether they're on, and through what"},
			{"meko answers off", "turn them off"},
		}},
	{name: groupAgent, subcommands: true, summary: "List the agents on this computer, and connect them to Meko",
		usage: "meko agent list|connect|disconnect [<agent>] [flags]", examples: []example{
			{"meko agent list", "the agents on this computer, and whether each one reaches Meko"},
			{"meko agent connect claude", "add Meko's MCP server to Claude Code, for all your projects"},
		}},
	{name: "agent list", group: "Connect", summary: "Show the agents on this computer and whether each reaches Meko",
		usage: "meko agent list [flags]", examples: []example{
			{"meko agent list", "a row for each agent installed, and where agents connect"},
			{"meko agent list --json", "the same, for scripts and extensions"},
		}},
	{name: "agent connect", group: "Connect", summary: "Connect an agent to Meko (claude, codex, kiro, opencode, cursor)",
		usage: "meko agent connect [claude|codex|kiro|opencode|cursor]", examples: []example{
			{"meko agent connect claude", "add Meko's MCP server to Claude Code, for all your projects"},
			{"meko agent connect kiro", "the same for Kiro CLI (codex, opencode and cursor too)"},
			{"meko agent connect", "the MCP URL and token, to connect any MCP client"},
		}},
	{name: "agent disconnect", group: "Connect", summary: "Disconnect an agent from Meko",
		usage: "meko agent disconnect claude|codex|kiro|opencode|cursor", examples: []example{
			{"meko agent disconnect claude", "remove Meko's MCP server from Claude Code"},
		}},
	{name: "token", group: "Connect", summary: "Print the token agents send",
		usage: "meko token [flags]", examples: []example{
			{"meko token", "the token MCP and REST clients send (Authorization: Bearer <token>)"},
			{"meko token --json", "the MCP URL, the token, whether Meko runs and the Meko Cloud link"},
		}},
	{name: "cloud-link", group: "Connect", summary: "Link Meko Cloud, or unlink it",
		usage: "meko cloud-link [flags]", examples: []example{
			{"meko cloud-link", "link Meko Cloud for the extensions: sign in with your browser, or enter an API key"},
			{"echo \"$KEY\" | meko cloud-link", "link with the API key on stdin"},
			{"meko cloud-link --off", "unlink it: the extensions use this Meko"},
			{"meko cloud-link --url https://meko.example.com/mcp", "link another Meko server"},
		}},
	{name: groupExtension, subcommands: true, summary: "List, install, update and delete Meko's extensions",
		usage: "meko extension list|install|update|delete [<name>] [flags]", examples: []example{
			{"meko extension list", "the extensions, installed or not: running, their version and updates"},
			{"meko extension install agentk", "install AgentK from its newest release, and run it with Meko"},
		}},
	{name: "extension list", group: "Extensions",
		summary: "Show the extensions: installed or not, running, their version and updates",
		usage:   "meko extension list [flags]", examples: []example{
			{"meko extension list", "a row each: its state, the release installed and its updates"},
			{"meko extension list --json", "the same, for scripts"},
		}},
	{name: "extension install", group: "Extensions", summary: "Add an extension (agentk, present)",
		usage: "meko extension install agentk|present [flags] [-- <its install's options>]", examples: []example{
			{"meko extension install agentk", "install AgentK from its newest release, and run it with Meko"},
			{"meko extension install agentk -- --agent opencode", "pass options on to AgentK's own install"},
		}},
	{name: "extension update", group: "Extensions", summary: "Update an extension now, or turn its automatic updates off",
		usage: "meko extension update agentk|present|--all [flags]", examples: []example{
			{"meko extension update --all", "update every extension now"},
			{"meko extension update agentk --check", "only look"},
			{"meko extension update agentk --auto=off", "stop Meko installing its updates on its own"},
		}},
	{name: "extension delete", group: "Extensions", summary: "Remove an extension (--purge also deletes its data)",
		usage: "meko extension delete agentk|present [flags]", examples: []example{
			{"meko extension delete present", "stop it and remove it, after asking; its data stays"},
			{"meko extension delete agentk --purge", "also delete its data: AgentK's sessions and worktrees"},
			{"meko extension delete agentk --force", "don't ask"},
		}},
	{name: "kode", group: "Extensions", summary: "Run one of AgentK's commands",
		usage: "meko kode <command> [args]", examples: []example{
			{"meko kode ui", "open AgentK in your browser"},
			{"meko kode status", "whether AgentK is running, and its address"},
			{"meko kode attach <session>", "show a session's terminal in yours (Ctrl+\\ detaches)"},
			{"meko kode --help", "AgentK's commands"},
		}},
	{name: "present", group: "Extensions", summary: "Run one of Presenter's commands",
		usage: "meko present <command> [args]", examples: []example{
			{"meko present status", "whether Presenter is running"},
			{"meko present --help", "Presenter's commands"},
		}},
	{name: "destroy", group: "Remove", summary: "Stop Meko and delete its data",
		usage: "meko destroy [flags]", examples: []example{
			{"meko destroy", "stop Meko and delete its data folder for good, after asking"},
			{"meko destroy --with-extensions", "also uninstall the extensions and delete their data"},
			{"meko destroy --force", "don't ask (keeps the extensions' data)"},
		}},
	{name: "uninstall", group: "Remove", summary: "Remove Meko and its extensions, keeping their data",
		usage: "meko uninstall [flags]", examples: []example{
			{"meko uninstall", "remove the extensions, then the service, the releases and the commands; the data stays"},
			{"meko uninstall --purge", "also delete the data, the extensions' too, and its secrets, after asking"},
		}},
	{name: "serve", hidden: true, summary: "Run the server in the foreground",
		usage: "meko serve [flags]", examples: []example{
			{"meko serve", "what the service runs"},
		}},
}

var mekoCLIGroups = []string{"Datapacks", "Memory and knowledge"}

var mekoCLICommands = []command{
	{name: "datapacks", group: "Datapacks", summary: "List the datapacks",
		usage: "meko-cli datapacks [flags]", examples: []example{
			{"meko-cli datapacks", "by name, with their descriptions: Meko Cloud's and this computer's while it's linked"},
			{"meko-cli datapacks --json", "with each one's id"},
		}},
	{name: "create_datapack", group: "Datapacks", summary: "Create a datapack",
		usage: "meko-cli create_datapack <name> [flags]", examples: []example{
			{"meko-cli create_datapack research", "create the datapack research: in the linked Meko Cloud while it's linked"},
		}},
	{name: "delete_datapack", group: "Datapacks", summary: "Delete a datapack and everything in it",
		usage: "meko-cli delete_datapack <name|id> [flags]", examples: []example{
			{"meko-cli delete_datapack research", "delete it and everything in it, for good, after asking"},
			{"meko-cli delete_datapack research --force", "don't ask"},
		}},
	{name: "export", group: "Datapacks", summary: "Write one of this computer's datapacks to a zip",
		usage: "meko-cli export [<name|id>] [flags]", examples: []example{
			{"meko-cli export", "write the default datapack to meko-<name>-<date>.zip here"},
			{"meko-cli export research -o research.zip", "write another datapack to a file you name"},
		}},
	{name: "search", group: "Memory and knowledge", summary: "Search memories, files and past conversations",
		usage: `meko-cli search "<question>" [flags]`, examples: []example{
			{`meko-cli search "why is the web listener separate"`, "search the default datapack"},
			{`meko-cli search "release steps" --source knowledge_base --limit 3`, "only the knowledge base, three results"},
			{`meko-cli search "owner's login" --datapack research --json`, "another datapack, the tool's raw reply"},
		}},
	{name: "add_memory", group: "Memory and knowledge", summary: "Save a memory, word for word",
		usage: `meko-cli add_memory "<text>" [flags]`, examples: []example{
			{`meko-cli add_memory "CI runs on Linux and macOS"`, "save it to the default datapack"},
			{"git log -1 --format=%B | meko-cli add_memory", "read the text from stdin"},
		}},
	{name: "upload", group: "Memory and knowledge", summary: "Add files to the knowledge base",
		usage: "meko-cli upload <file>... [flags]", examples: []example{
			{"meko-cli upload notes.md spec.pdf", "add the files and wait until they're indexed"},
			{"meko-cli upload big.pdf --no-wait", "return once it's uploaded"},
		}},
	{name: "upload_status", group: "Memory and knowledge", summary: "Show how an uploaded file is doing",
		usage: "meko-cli upload_status <key> [flags]", examples: []example{
			{"meko-cli upload_status <key>", "the key upload printed"},
		}},
}

// commandsOf are prog's commands.
func commandsOf(prog string) []command {
	if prog == progMekoCLI {
		return mekoCLICommands
	}
	return mekoCommands
}

// lookup finds prog's command called name.
func lookup(prog, name string) (command, bool) {
	for _, c := range commandsOf(prog) {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// startHere prints what to type next for the state Meko is in on this computer, after meko's
// command list, so someone running meko for the first time isn't left to pick from all of it. The
// commands are bold on a terminal.
func startHere(w io.Writer) {
	root, err := paths.Resolve("")
	if err != nil {
		return
	}
	l := paths.New(root)
	var title string
	var cmds [][2]string
	if up, _ := running(l.Lock); up {
		title = "Next:"
		c, _ := newClient(l)
		cmds = nextSteps(readNextState(l, c), nextUse)
	} else if _, err = os.Stat(l.Root); err == nil {
		title = "Meko is set up but not running. Start it with:"
		cmds = [][2]string{{"meko start", "start Meko, then meko ui opens the web UI"}}
	} else {
		title = "New here? Start with:"
		cmds = [][2]string{{"meko install", "set Meko up and start it, now and at every login"},
			{"meko ui", "then open the web UI"}}
	}
	bold := func(s string) string { return s }
	if spin.Color(w) {
		bold = func(s string) string { return "\033[1m" + s + "\033[0m" }
	}
	fmt.Fprintf(w, "\n%s\n", title)
	width := 0
	for _, c := range cmds {
		width = max(width, len(c[0]))
	}
	for _, c := range cmds {
		fmt.Fprintf(w, "  %s%s    %s\n", bold(c[0]), strings.Repeat(" ", width-len(c[0])), c[1])
	}
}

// printUsage prints prog's commands, by group; with start, meko's ends with what to type next
// (startHere).
func printUsage(w io.Writer, prog string, start bool) {
	groups := mekoGroups
	if prog == progMekoCLI {
		groups = mekoCLIGroups
		fmt.Fprint(w, "meko-cli works with what Meko keeps: datapacks, memories, files and conversations\n"+
			"\nUsage: meko-cli <command> [flags]\n")
	} else {
		fmt.Fprint(w, "The Context Engine for Agentic Applications\n\nUsage: meko <command> [flags]\n")
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s\n", g)
		for _, c := range commandsOf(prog) {
			if c.group == g && !c.hidden {
				fmt.Fprintf(w, "  %-22s%s\n", c.name, c.summary)
			}
		}
	}
	if prog == progMekoCLI {
		fmt.Fprint(w, "\nCommands take --datapack NAME|ID where it applies, and --json for the raw reply\n"+
			"Run meko-cli <command> --help for examples. Meko itself: meko --help\n")
		fmt.Fprintf(w, "Docs: %s#meko-cli\n", docsURL)
		return
	}
	fmt.Fprint(w, "\nTo work with datapacks, memories and files, run meko-cli\n")
	fmt.Fprintf(w, "Run meko <command> --help for examples and flags. Docs: %s\n", docsURL)
	if start {
		startHere(w)
	}
}

// printCommandHelp prints one command's summary, usage, examples and flags (fs's, when it has any).
func printCommandHelp(w io.Writer, prog string, c command, fs *flag.FlagSet) {
	r, n := utf8.DecodeRuneInString(c.summary)
	fmt.Fprintf(w, "%s %s: %c%s\n\nUsage: %s\n", prog, c.name, unicode.ToLower(r), c.summary[n:], c.usage)
	fmt.Fprintf(w, "\nExamples:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 4, ' ', 0)
	for _, e := range c.examples {
		fmt.Fprintf(tw, "  %s\t%s\n", e.cmd, e.note)
	}
	_ = tw.Flush()
	if fs != nil {
		printFlags(w, fs)
	}
	anchor := strings.ReplaceAll(c.name, " ", "-")
	if prog == progMekoCLI {
		anchor = "meko-cli-" + c.name
	}
	fmt.Fprintf(w, "\nMore: %s#%s\n", docsURL, anchor)
}

// printGroupHelp prints a command's own help and its subcommands (meko extension's, meko agent's).
func printGroupHelp(w io.Writer, prog, group string) {
	c, _ := lookup(prog, group)
	r, n := utf8.DecodeRuneInString(c.summary)
	fmt.Fprintf(w, "%s %s: %c%s\n\nUsage: %s\n\nCommands:\n", prog, c.name, unicode.ToLower(r), c.summary[n:], c.usage)
	tw := tabwriter.NewWriter(w, 0, 0, 4, ' ', 0)
	for _, sub := range commandsOf(prog) {
		if name, ok := strings.CutPrefix(sub.name, group+" "); ok {
			fmt.Fprintf(tw, "  %s\t%s\n", name, sub.summary)
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nExamples:\n")
	tw = tabwriter.NewWriter(w, 0, 0, 4, ' ', 0)
	for _, e := range c.examples {
		fmt.Fprintf(tw, "  %s\t%s\n", e.cmd, e.note)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nRun %s %s <command> --help for their examples and flags\n", prog, group)
	fmt.Fprintf(w, "More: %s#%s\n", docsURL, group)
}

// unknownSubcommand says sub isn't one of group's subcommands.
func unknownSubcommand(prog, group, sub string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "%s %s: unknown command %q\n", prog, group, sub)
	fmt.Fprintf(stderr, "Run %s %s --help to see its commands\n", prog, group)
	return exitUsage
}

// flagValueNames name a flag's value in help, where the flag's type alone would say string or int.
var flagValueNames = map[string]string{
	"data-dir": "DIR", "log-level": "LEVEL", "datapack": "NAME|ID", "o": "FILE", "n": "N", "port": "N",
	"limit": "N", "source": "SOURCE", "access": "local|remote", "auto": "on|off", "version": "V",
	"url": "URL", "web-url": "URL", "api-url": "URL",
}

// printFlags prints fs's flags, --data-dir last since every command takes it. Flags only the
// service or tests pass (serve's --detached) aren't shown.
func printFlags(w io.Writer, fs *flag.FlagSet) {
	var rows, last [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "detached" {
			return
		}
		kind, usage := flag.UnquoteUsage(f)
		left := "--" + f.Name
		if len(f.Name) == 1 {
			left = "-" + f.Name
		}
		if kind != "" {
			name, ok := flagValueNames[f.Name]
			if !ok {
				name = strings.ToUpper(kind)
			}
			left += " " + name
		}
		if d := f.DefValue; d != "" && d != "false" && d != "0" && !strings.Contains(usage, "default") {
			usage += " (default " + d + ")"
		}
		if f.Name == "data-dir" {
			last = append(last, [2]string{left, usage})
		} else {
			rows = append(rows, [2]string{left, usage})
		}
	})
	rows = append(rows, last...)
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\nFlags:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 4, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\n", r[0], r[1])
	}
	_ = tw.Flush()
}

// nearest is the command of prog's whose name is closest to name, or "" when none is close: within
// two edits, or one that starts with it.
func nearest(prog, name string) string {
	best, bestD := "", 3
	for _, c := range commandsOf(prog) {
		if c.hidden {
			continue
		}
		if len(name) >= 3 && strings.HasPrefix(c.name, name) {
			return c.name
		}
		if d := editDistance(name, c.name); d < bestD {
			best, bestD = c.name, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// shellQuote joins args as a shell would read them back: each word as it is when it's safe, else in
// single quotes.
func shellQuote(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		safe := a != ""
		for _, r := range a {
			if r >= utf8.RuneSelf || !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_@%+=:,./-", r) {
				safe = false
				break
			}
		}
		if safe {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}
