package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// Every command has a group (one its program lists), a summary, a usage line and an example, and
// each --help exits 0 with them. A two-word command (extension list) runs as its two words.
func TestEveryCommandHasHelp(t *testing.T) {
	for _, prog := range []string{progMeko, progMekoCLI} {
		groups := mekoGroups
		if prog == progMekoCLI {
			groups = mekoCLIGroups
		}
		var usage bytes.Buffer
		printUsage(&usage, prog, false)
		for _, c := range commandsOf(prog) {
			if c.summary == "" || c.usage == "" || len(c.examples) == 0 {
				t.Errorf("%s %s: summary %q, usage %q, %d examples", prog, c.name, c.summary, c.usage, len(c.examples))
			}
			if !c.hidden && !c.subcommands && !slices.Contains(groups, c.group) {
				t.Errorf("%s %s: group %q isn't one %s --help lists", prog, c.name, c.group, prog)
			}
			// A command with subcommands is listed by them.
			listed := strings.Contains(usage.String(), "\n  "+fmt.Sprintf("%-22s", c.name))
			if listed == (c.hidden || c.subcommands) {
				t.Errorf("%s %s: listed in --help %v, hidden %v", prog, c.name, listed, c.hidden)
			}
			var out, errb bytes.Buffer
			code := runProg(prog, append(strings.Fields(c.name), "--help"), &out, &errb)
			if code != exitOK || !strings.Contains(out.String(), "Usage: "+c.usage) ||
				!strings.Contains(out.String(), c.examples[0].cmd) || errb.Len() != 0 {
				t.Errorf("%s %s --help: exit %d, output %q, errors %q", prog, c.name, code, out.String(), errb.String())
			}
		}
	}
}

// AgentK's movehome.ts tells a Meko that keeps the extensions' data by --with-extensions in
// `meko destroy -h`, stdout and stderr together.
func TestDestroyHelpNamesWithExtensions(t *testing.T) {
	code, out, errs := run("destroy", "-h")
	want(t, "destroy -h", code, 0, out+errs, "--with-extensions")
	_, help, _ := run("help", "destroy")
	if help != out {
		t.Errorf("meko help destroy %q != meko destroy -h %q", help, out)
	}
}

func TestUsage(t *testing.T) {
	t.Setenv(paths.HomeEnv, t.TempDir()) // meko's usage looks at the data folder to say where to start
	code, out, _ := run()
	if !strings.HasPrefix(out, "The Context Engine for Agentic Applications\n\n") {
		t.Errorf("meko --help doesn't open with Meko's tagline:\n%s", out)
	}
	want(t, "no command", code, 0, out, "Usage: meko <command> [flags]")
	want(t, "no command", code, 0, out, "run meko-cli\n")
	if strings.Contains(out, "serve") || strings.Contains(out, "search") {
		t.Errorf("meko --help lists a hidden or meko-cli command:\n%s", out)
	}
	code, out, _ = run("version")
	want(t, "version", code, 0, out, "sqlite-vec  v0.1.9")
	_, flag, _ := run("--version")
	if flag != out {
		t.Errorf("--version %q, version %q", flag, out)
	}

	code, _, errs := run("frobnicate")
	want(t, "unknown command", code, exitUsage, errs, `meko: unknown command "frobnicate"`+"\nRun meko --help")
	code, _, errs = run("statsu")
	want(t, "a typo", code, exitUsage, errs, "Did you mean meko status?\n")
	// cloud_link was renamed cloud-link: the old name runs nothing and points to the new one.
	code, _, errs = run("cloud_link", "--off")
	want(t, "cloud_link", code, exitUsage, errs, `meko: unknown command "cloud_link"`+"\nDid you mean meko cloud-link?\n")
	code, out, _ = run("cloud-link", "-h")
	want(t, "cloud-link -h", code, 0, out, "Usage: meko cloud-link [flags]")
	want(t, "cloud-link -h", code, 0, out, "More: "+docsURL+"#cloud-link\n")
	code, _, errs = run("start", "--nope")
	want(t, "a bad flag", code, exitUsage, errs, "Run meko start --help for its flags\n")
	code, _, errs = run("help", "frobnicate")
	want(t, "help for an unknown command", code, exitUsage, errs, `unknown command "frobnicate"`)

	code, out, _ = runSh("--help")
	want(t, "meko-cli --help", code, 0, out, "\nUsage: meko-cli <command> [flags]\n")
	// meko-cli runs one command a run: alone it prints its help and reads nothing, a pipe included.
	piped := strings.NewReader("datapacks\n")
	old := stdin
	stdin = piped
	t.Cleanup(func() { stdin = old })
	code, alone, errs := runSh()
	if code != 0 || alone != out || errs != "" {
		t.Errorf("meko-cli alone: exit %d, %q, %q; want its --help %q", code, alone, errs, out)
	}
	if piped.Len() != len("datapacks\n") {
		t.Errorf("meko-cli alone read %d bytes of stdin", len("datapacks\n")-piped.Len())
	}
	for _, gone := range []string{"\n  use ", "\n  exit ", "\n  help ", "prompt"} {
		if strings.Contains(out, gone) {
			t.Errorf("meko-cli --help still has %q:\n%s", gone, out)
		}
	}
	code, out, _ = runSh("help", "search")
	want(t, "meko-cli help search", code, 0, out, "meko-cli search: search memories")
	for _, cmd := range []string{"use", "exit", "quit"} {
		code, _, errs = runSh(cmd, "research")
		want(t, "meko-cli "+cmd, code, exitUsage, errs, "meko-cli: unknown command \""+cmd+"\"\n")
	}
	code, _, errs = runSh("--data-dir", t.TempDir())
	want(t, "meko-cli with only --data-dir", code, exitUsage, errs, "meko-cli: unknown command \"--data-dir\"\n")
	code, _, errs = runSh("start")
	want(t, "meko-cli start", code, exitUsage, errs, "meko-cli: start is a meko command: meko start\n")
}

// meko's usage commands are meko-cli's now: meko names the command to run instead, and runs nothing.
func TestMekoNamesTheMekoCLICommand(t *testing.T) {
	for _, name := range []string{"search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload",
		"upload_status", "export"} {
		code, out, errs := run(name, "--data-dir", "/nonexistent", "two words")
		if code != exitUsage || out != "" ||
			errs != "meko: "+name+" is a meko-cli command now: meko-cli "+name+" --data-dir /nonexistent 'two words'\n" {
			t.Errorf("meko %s: exit %d, output %q, errors %q", name, code, out, errs)
		}
	}
}

// meko extension groups the extension commands: alone it lists them (exit 2), help names each, and
// the snake_case commands it replaced are gone, with no pointer to it. The plural is a typo of it.
func TestExtensionCommands(t *testing.T) {
	code, out, _ := run("extension")
	want(t, "extension alone", code, exitUsage, out, "Usage: meko extension list|install|update|delete")
	for _, sub := range []string{"list", "install", "update", "delete"} {
		want(t, "extension alone", code, exitUsage, out, "\n  "+sub+" ")
	}
	code, help, _ := run("help", "extension")
	if code != exitOK || help != out {
		t.Errorf("meko help extension: exit %d, %q", code, help)
	}
	code, out, _ = run("extension", "delete", "-h")
	want(t, "extension delete -h", code, 0, out, "Usage: meko extension delete agentk|presenter [flags]")
	want(t, "extension delete -h", code, 0, out, "--purge")
	want(t, "extension delete -h", code, 0, out, "More: "+docsURL+"#extension-delete")
	_, help, _ = run("help", "extension", "delete")
	if help != out {
		t.Errorf("meko help extension delete %q != meko extension delete -h %q", help, out)
	}
	_, usage, _ := run()
	want(t, "meko --help", 0, 0, usage, "\nExtensions\n  extension list ")
	for _, old := range []string{"install_extension", "update_extension", "uninstall_extension"} {
		if strings.Contains(usage, old) {
			t.Errorf("meko --help lists %s", old)
		}
		oldCode, _, oldErrs := run(old, "agentk")
		want(t, old, oldCode, exitUsage, oldErrs, `meko: unknown command "`+old+`"`)
		if strings.Contains(oldErrs, "Did you mean") {
			t.Errorf("%s points somewhere: %q", old, oldErrs)
		}
	}
	code, _, errs := run("extensions", "list")
	want(t, "extensions list", code, exitUsage, errs, "Did you mean meko extension list?")
	code, _, errs = run("extension", "frob")
	want(t, "extension frob", code, exitUsage, errs, `meko extension: unknown command "frob"`)
	code, _, errs = run("extension list")
	want(t, "one word with a space", code, exitUsage, errs, `unknown command "extension list"`)
	code, _, errs = run("extension", "delete", "--data-dir", t.TempDir())
	want(t, "extension delete with no name", code, exitUsage, errs, "name the extension (agentk, presenter)")
	code, _, errs = run("extension", "delete", "frob", "--data-dir", t.TempDir())
	want(t, "extension delete frob", code, exitUsage, errs, `no extension called "frob"`)
}

func TestMainPicksTheProgramByName(t *testing.T) {
	t.Setenv(paths.HomeEnv, t.TempDir())
	var out, errb bytes.Buffer
	Main("/home/me/.local/bin/meko-cli", []string{"--help"}, &out, &errb)
	want(t, "as meko-cli", 0, 0, out.String(), "Usage: meko-cli")
	out.Reset()
	Main("/opt/meko/bin/meko", []string{"--help"}, &out, &errb)
	want(t, "as meko", 0, 0, out.String(), "Usage: meko <command>")
	out.Reset()
	code := Main("/home/me/.local/bin/mekosh", []string{"search", "two words"}, &out, &errb)
	want(t, "as mekosh", code, exitUsage, errb.String(), "mekosh is meko-cli now: meko-cli search 'two words'\n")
	if out.Len() != 0 {
		t.Errorf("as mekosh, output %q", out.String())
	}
	// mekosh took --data-dir before the command; the meko-cli command it names takes it after, so it runs.
	for args, cmd := range map[string]string{
		"--data-dir /x search q":  "meko-cli search q --data-dir /x",
		"--data-dir=/x datapacks": "meko-cli datapacks --data-dir=/x",
		"-data-dir /x":            "meko-cli",
		"search q --data-dir /x":  "meko-cli search q --data-dir /x",
		"--help":                  "meko-cli --help",
	} {
		errb.Reset()
		code = Main("/home/me/.local/bin/mekosh", strings.Fields(args), &out, &errb)
		want(t, "mekosh "+args, code, exitUsage, errb.String(), "mekosh is meko-cli now: "+cmd+"\n")
	}
	// The named command runs: with no server, datapacks says so rather than "unknown command".
	code, _, errs := runSh(mekoshArgs([]string{"--data-dir", shortDir(t), "datapacks"})...)
	want(t, "the command mekosh names", code, exitNotRunning, errs, "isn't running")
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote([]string{"meko-cli", "search", "it's here", "--limit", "3", ""}); got !=
		`meko-cli search 'it'\''s here' --limit 3 ''` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestReport(t *testing.T) {
	var b bytes.Buffer
	r := newReport(&b)
	if r.color {
		t.Error("color on a buffer")
	}
	r.step(stepWarn, "Memory engine", "starting")
	r.running("Meko 1.0 is running", "pid 7, up 2h")
	r.links([]linkRow{{"Web UI", "http://127.0.0.1:7777", "local, from this computer"},
		{"Agents", "http://127.0.0.1:7778/mcp", "MCP and API"}, {"Cloud", "", "not linked"}})
	r.next([][2]string{{"meko ui", "open the web UI"},
		{"meko status", "check on Meko at any time · meko uninstall removes it"}})
	if want := "  ! Memory engine   starting\n" +
		"\n  ● Meko 1.0 is running · pid 7, up 2h\n" +
		"\n  Web UI  http://127.0.0.1:7777 · local, from this computer\n" +
		"  Agents  http://127.0.0.1:7778/mcp · MCP and API\n" +
		"  Cloud   not linked\n" +
		"\nNext:\n" +
		"  meko ui        open the web UI\n" +
		"  meko status    check on Meko at any time · meko uninstall removes it\n"; b.String() != want {
		t.Errorf("printed:\n%s\nwant:\n%s", b.String(), want)
	}

	// In color: the palette's roles, and the first command as the call to action, its text lined up
	// with the commands below it.
	b.Reset()
	r.color = true
	t.Setenv(spin.BackgroundEnv, "dark") // never ask the developer's terminal
	r.running("Meko 1.0 is running", "")
	r.links([]linkRow{{"Agents", "http://127.0.0.1:7778/mcp", ""}})
	r.next([][2]string{{"meko ui", "open the web UI"}, {"meko status", "check on Meko · meko uninstall removes it"}})
	if want := "\n  \033[32m●\033[0m \033[1mMeko 1.0 is running\033[0m\n" +
		"\n  \033[2mAgents\033[0m  " + spin.Link("http://127.0.0.1:7778/mcp", true) + "\n" +
		"\nNext:\n" +
		" \033[1;97;44m meko ui \033[0m       open the web UI\n" +
		"  " + spin.Blue("meko status", true) + "    check on Meko" +
		"\033[2m · meko uninstall removes it\033[0m\n"; b.String() != want {
		t.Errorf("printed in color %q\nwant %q", b.String(), want)
	}
	if strings.Contains(b.String(), "│") {
		t.Error("a box")
	}
	t.Setenv("NO_COLOR", "1")
	if newReport(os.Stdout).color {
		t.Error("color with NO_COLOR")
	}
}

// An install from before meko-cli gets ~/.local/bin/meko-cli beside its meko, and keeps its mekosh
// link, which names meko-cli; one whose meko isn't there, or that has a meko-cli already, is left
// alone.
func TestLinkMekoCLI(t *testing.T) {
	home := t.TempDir()
	app := update.App{Root: filepath.Join(t.TempDir(), "app")}
	if err := os.MkdirAll(filepath.Join(app.Root, "1.0", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app.Exe("1.0"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	if err := app.Repoint("1.0"); err != nil {
		t.Fatal(err)
	}
	mekoCLI := filepath.Join(home, ".local", "bin", "meko-cli")
	if err := linkMekoCLI(app, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(mekoCLI); !os.IsNotExist(err) {
		t.Fatalf("meko-cli without a meko command: %v", err)
	}
	if _, err := app.LinkCommand(home); err != nil {
		t.Fatal(err)
	}
	mekosh, err := app.LinkCommandAs(home, "mekosh")
	if err != nil {
		t.Fatal(err)
	}
	if err = linkMekoCLI(app, home); err != nil {
		t.Fatal(err)
	}
	if target, rerr := os.Readlink(mekoCLI); rerr != nil || target != app.CurrentExe() {
		t.Fatalf("meko-cli -> %q, %v", target, rerr)
	}
	if target, rerr := os.Readlink(mekosh); rerr != nil || target != app.CurrentExe() {
		t.Fatalf("mekosh -> %q, %v", target, rerr)
	}
	if err := app.UnlinkCommandAs(home, "meko-cli"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mekoCLI, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkMekoCLI(app, home); err != nil {
		t.Fatal(err)
	}
	if err := linkMekoCLI(app, home); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(mekoCLI); string(b) != "mine" {
		t.Errorf("a meko-cli that was there was replaced")
	}
}

// meko alone ends with what to type next, after its commands: install on a computer that has no
// Meko, start when it's set up but stopped.
func TestMekoAloneSaysWhereToStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meko")
	t.Setenv(paths.HomeEnv, dir)
	var out bytes.Buffer
	if code := Run(nil, &out, &out); code != exitOK ||
		!strings.Contains(out.String(), "Docs: "+docsURL+"\n\nNew here? Start with:\n  meko install") ||
		!strings.HasPrefix(out.String(), "The Context Engine for Agentic Applications\n\nUsage: meko ") {
		t.Errorf("no Meko yet: exit %d\n%s", code, out.String())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if Run([]string{"--help"}, &out, &out); !strings.Contains(out.String(), "\n\nMeko is set up but not running") ||
		!strings.Contains(out.String(), "  meko start") {
		t.Errorf("set up, stopped:\n%s", out.String())
	}
	out.Reset()
	if runProg(progMekoCLI, nil, &out, &out); strings.Contains(out.String(), "New here?") {
		t.Errorf("meko-cli's help says where Meko starts:\n%s", out.String())
	}
}
