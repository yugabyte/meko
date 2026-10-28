package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"
)

// The two programs this binary is: meko runs Meko, mekosh works with what it keeps. cmd/meko picks
// one from the name it was run by.
const (
	progMeko   = "meko"
	progMekosh = "mekosh"
)

// docsURL is where the CLI's docs live; each command's help links to its anchor there.
const docsURL = "https://mekodata.ai/docs/cli"

// command is one command's help: the group meko --help (or mekosh --help) lists it under, a one-line
// summary, its usage line and examples. Its flags come from its flag set. A hidden command runs but
// isn't listed; a promptOnly one is mekosh's, and only at its prompt.
type command struct {
	name, group, summary, usage string
	examples                    []example
	hidden, promptOnly          bool
}

type example struct{ cmd, note string }

var mekoGroups = []string{"Get started", "Run", "Connect", "Extensions", "Remove"}

var mekoCommands = []command{
	{name: "install", group: "Get started", summary: "Set Meko up and start it at login",
		usage: "meko install [flags]", examples: []example{
			{"meko install", "set Meko up with a login service (launchd or systemd), and start it"},
			{"meko install --access remote", "let browsers on other computers reach the web UI, over HTTPS with a login"},
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
		usage: "meko ui [path] [flags]", examples: []example{
			{"meko ui", "open Meko's web UI"},
			{"meko ui /ext/agentk/", "open AgentK's page"},
			{"meko ui --no-open", "only print the link (with remote access and no login yet, a setup link)"},
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
			{"meko logs", "the last 50 lines of the server's and the memory engine's logs"},
			{"meko logs -f", "follow them"},
			{"meko logs --memory-engine -n 200", "the memory engine's last 200 lines"},
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
	{name: "connect", group: "Connect", summary: "Connect a coding agent to Meko (claude, codex, kiro, opencode)",
		usage: "meko connect [claude|codex|kiro|opencode]", examples: []example{
			{"meko connect claude", "add Meko's MCP server to Claude Code, for all your projects"},
			{"meko connect kiro", "the same for Kiro CLI (codex and opencode too)"},
			{"meko connect", "the MCP URL and token, to connect any MCP client"},
		}},
	{name: "token", group: "Connect", summary: "Print the token agents send",
		usage: "meko token [flags]", examples: []example{
			{"meko token", "the token MCP and REST clients send (Authorization: Bearer <token>)"},
			{"meko token --json", "the MCP URL, the token, whether Meko runs and the Meko Cloud link"},
		}},
	{name: "cloud_link", group: "Connect", summary: "Link Meko Cloud, or unlink it",
		usage: "meko cloud_link [flags]", examples: []example{
			{"meko cloud_link", "link Meko Cloud for the extensions: asks for the API key, or reads it from stdin"},
			{"meko cloud_link --off", "unlink it: the extensions use this Meko"},
			{"meko cloud_link --url https://meko.example.com/mcp", "link another Meko server"},
		}},
	{name: "setup_link", group: "Connect", summary: "Make or reset the web UI's login for remote access",
		usage: "meko setup_link", examples: []example{
			{"meko setup_link", "print a one-time link that sets up the login, or replaces a lost one"},
		}},
	{name: "install_extension", group: "Extensions", summary: "Add an extension (agentk, presenter)",
		usage: "meko install_extension agentk|presenter [flags] [-- <its install's options>]", examples: []example{
			{"meko install_extension agentk", "install AgentK from its newest release, and run it with Meko"},
			{"meko install_extension agentk -- --agent opencode", "pass options on to AgentK's own install"},
		}},
	{name: "update_extension", group: "Extensions", summary: "Update an extension now, or turn its automatic updates off",
		usage: "meko update_extension agentk|presenter|--all [flags]", examples: []example{
			{"meko update_extension --all", "update every extension now"},
			{"meko update_extension agentk --check", "only look"},
			{"meko update_extension agentk --auto=off", "stop Meko installing its updates on its own"},
		}},
	{name: "uninstall_extension", group: "Extensions", summary: "Stop running an extension with Meko",
		usage: "meko uninstall_extension agentk|presenter", examples: []example{
			{"meko uninstall_extension presenter", "meko start and stop no longer run it (it stays installed)"},
		}},
	{name: "destroy", group: "Remove", summary: "Stop Meko and delete its data",
		usage: "meko destroy [flags]", examples: []example{
			{"meko destroy", "stop Meko and delete its data folder for good, after asking"},
			{"meko destroy --with-extensions", "also uninstall the extensions and delete their data"},
			{"meko destroy --force", "don't ask (keeps the extensions' data)"},
		}},
	{name: "uninstall", group: "Remove", summary: "Remove Meko, keeping its data",
		usage: "meko uninstall [flags]", examples: []example{
			{"meko uninstall", "remove the service, the releases and the commands; your data stays"},
			{"meko uninstall --purge", "also delete the data and its secrets, as destroy does"},
		}},
	{name: "serve", hidden: true, summary: "Run the server in the foreground",
		usage: "meko serve [flags]", examples: []example{
			{"meko serve", "what the service runs"},
		}},
}

var mekoshGroups = []string{"Datapacks", "Memory and knowledge", "At the prompt only"}

var mekoshCommands = []command{
	{name: "datapacks", group: "Datapacks", summary: "List the datapacks, and the linked Meko Cloud's",
		usage: "mekosh datapacks [flags]", examples: []example{
			{"mekosh datapacks", "this Meko's and the linked Meko Cloud's, by name, with their descriptions"},
			{"mekosh datapacks --local", "only this Meko's"},
			{"mekosh datapacks --json", "with each one's id, and whether it's this Meko's or Meko Cloud's (and shared)"},
		}},
	{name: "create_datapack", group: "Datapacks", summary: "Create a datapack",
		usage: "mekosh create_datapack <name> [flags]", examples: []example{
			{"mekosh create_datapack research", "create the datapack research"},
			{"mekosh create_datapack team-notes --cloud", "create it in the linked Meko Cloud"},
		}},
	{name: "delete_datapack", group: "Datapacks", summary: "Delete a datapack and everything in it",
		usage: "mekosh delete_datapack <name|id> [flags]", examples: []example{
			{"mekosh delete_datapack research", "delete it and everything in it, for good, after asking"},
			{"mekosh delete_datapack research --force", "don't ask"},
			{"mekosh delete_datapack cloud:team-notes", "the linked Meko Cloud's, when this Meko has one named so too"},
		}},
	{name: "export", group: "Datapacks", summary: "Write a datapack to a zip",
		usage: "mekosh export [<name|id>] [flags]", examples: []example{
			{"mekosh export", "write the default datapack to meko-<name>-<date>.zip here"},
			{"mekosh export research -o research.zip", "write another datapack to a file you name"},
		}},
	{name: "search", group: "Memory and knowledge", summary: "Search memories, files and past conversations",
		usage: `mekosh search "<question>" [flags]`, examples: []example{
			{`mekosh search "why is the web listener separate"`, "search the default datapack"},
			{`mekosh search "release steps" --source knowledge_base --limit 3`, "only the knowledge base, three results"},
			{`mekosh search "owner's login" --datapack research --json`, "another datapack, the tool's raw reply"},
			{`mekosh search "deploy steps" --datapack cloud:team-runbooks`, "a datapack in the linked Meko Cloud"},
		}},
	{name: "add_memory", group: "Memory and knowledge", summary: "Save a memory, word for word",
		usage: `mekosh add_memory "<text>" [flags]`, examples: []example{
			{`mekosh add_memory "CI runs on Linux and macOS"`, "save it to the default datapack"},
			{"git log -1 --format=%B | mekosh add_memory", "read the text from stdin (not at the prompt)"},
		}},
	{name: "upload", group: "Memory and knowledge", summary: "Add files to the knowledge base",
		usage: "mekosh upload <file>... [flags]", examples: []example{
			{"mekosh upload notes.md spec.pdf", "add the files and wait until they're indexed"},
			{"mekosh upload big.pdf --no-wait", "return once it's uploaded"},
		}},
	{name: "upload_status", group: "Memory and knowledge", summary: "Show how an uploaded file is doing",
		usage: "mekosh upload_status <key> [flags]", examples: []example{
			{"mekosh upload_status <key>", "the key upload printed"},
		}},
	{name: "use", group: "At the prompt only", promptOnly: true, summary: "Set the datapack later commands use",
		usage: "use [<name|id>]", examples: []example{
			{"use research", "search, add_memory, upload and upload_status use research unless a line names another"},
			{"use cloud:team-notes", "a datapack in the linked Meko Cloud (cloud: only when this Meko has one named so)"},
			{"use", "print the datapack in use"},
		}},
	{name: "help", group: "At the prompt only", promptOnly: true, summary: "This list, or help <command>",
		usage: "help [<command>]", examples: []example{{"help search", "search's examples and flags"}}},
	{name: "exit", group: "At the prompt only", promptOnly: true, summary: "Leave (also quit, or Ctrl-D)",
		usage: "exit", examples: []example{{"exit", "leave the prompt"}}},
}

// commandsOf are prog's commands.
func commandsOf(prog string) []command {
	if prog == progMekosh {
		return mekoshCommands
	}
	return mekoCommands
}

// lookup finds prog's command called name.
func lookup(prog, name string) (command, bool) {
	if prog == progMekosh && name == "quit" {
		name = "exit"
	}
	for _, c := range commandsOf(prog) {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// printUsage prints prog's commands, by group.
func printUsage(w io.Writer, prog string) {
	groups := mekoGroups
	if prog == progMekosh {
		groups = mekoshGroups
		fmt.Fprint(w, "mekosh works with what Meko keeps: datapacks, memories, files and conversations\n"+
			"Run it alone for a prompt, or give it one command\n\nUsage: mekosh [<command> [flags]]\n")
	} else {
		fmt.Fprint(w, "The universal, self-learning context engine for agentic applications\n\n"+
			"Usage: meko <command> [flags]\n")
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s\n", g)
		for _, c := range commandsOf(prog) {
			if c.group == g && !c.hidden {
				fmt.Fprintf(w, "  %-22s%s\n", c.name, c.summary)
			}
		}
	}
	if prog == progMekosh {
		fmt.Fprint(w, "\nCommands take --datapack NAME|ID where it applies, and --json for the raw reply\n"+
			"Run mekosh <command> --help for examples. Meko itself: meko --help\n")
		fmt.Fprintf(w, "Docs: %s#mekosh\n", docsURL)
		return
	}
	fmt.Fprint(w, "\nTo work with datapacks, memories and files, run mekosh\n")
	fmt.Fprintf(w, "Run meko <command> --help for examples and flags. Docs: %s\n", docsURL)
}

// printCommandHelp prints one command's summary, usage, examples and flags (fs's, when it has any).
func printCommandHelp(w io.Writer, prog string, c command, fs *flag.FlagSet) {
	r, n := utf8.DecodeRuneInString(c.summary)
	fmt.Fprintf(w, "%s %s: %c%s\n\nUsage: %s\n", prog, c.name, unicode.ToLower(r), c.summary[n:], c.usage)
	if prog == progMekosh && c.promptOnly {
		fmt.Fprintf(w, "\nExamples, at the mekosh prompt:\n")
	} else {
		fmt.Fprintf(w, "\nExamples:\n")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 4, ' ', 0)
	for _, e := range c.examples {
		fmt.Fprintf(tw, "  %s\t%s\n", e.cmd, e.note)
	}
	_ = tw.Flush()
	if fs != nil {
		printFlags(w, fs)
	}
	anchor := c.name
	if prog == progMekosh {
		anchor = "mekosh-" + c.name
	}
	fmt.Fprintf(w, "\nMore: %s#%s\n", docsURL, anchor)
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
		if c.hidden || c.promptOnly {
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
