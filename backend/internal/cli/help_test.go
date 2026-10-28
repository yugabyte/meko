package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yugabyte/meko/backend/internal/update"
)

// Every command has a group (one its program lists), a summary, a usage line and an example, and
// each --help exits 0 with them.
func TestEveryCommandHasHelp(t *testing.T) {
	for _, prog := range []string{progMeko, progMekosh} {
		groups := mekoGroups
		if prog == progMekosh {
			groups = mekoshGroups
		}
		var usage bytes.Buffer
		printUsage(&usage, prog)
		for _, c := range commandsOf(prog) {
			if c.summary == "" || c.usage == "" || len(c.examples) == 0 {
				t.Errorf("%s %s: summary %q, usage %q, %d examples", prog, c.name, c.summary, c.usage, len(c.examples))
			}
			if !c.hidden && !slices.Contains(groups, c.group) {
				t.Errorf("%s %s: group %q isn't one %s --help lists", prog, c.name, c.group, prog)
			}
			listed := strings.Contains(usage.String(), "\n  "+c.name+" ")
			if listed == c.hidden {
				t.Errorf("%s %s: listed in --help %v, hidden %v", prog, c.name, listed, c.hidden)
			}
			if c.promptOnly {
				continue
			}
			var out, errb bytes.Buffer
			code := runProg(prog, []string{c.name, "--help"}, &out, &errb)
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
	code, out, _ := run()
	want(t, "no command", code, 0, out, "Usage: meko <command> [flags]")
	want(t, "no command", code, 0, out, "run mekosh\n")
	if strings.Contains(out, "serve") || strings.Contains(out, "search") {
		t.Errorf("meko --help lists a hidden or mekosh command:\n%s", out)
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
	code, _, errs = run("start", "--nope")
	want(t, "a bad flag", code, exitUsage, errs, "Run meko start --help for its flags\n")
	code, _, errs = run("help", "frobnicate")
	want(t, "help for an unknown command", code, exitUsage, errs, `unknown command "frobnicate"`)

	code, out, _ = runSh("--help")
	want(t, "mekosh --help", code, 0, out, "Usage: mekosh [<command> [flags]]")
	code, out, _ = runSh("help", "use")
	want(t, "mekosh help use", code, 0, out, "Examples, at the mekosh prompt:")
	code, _, errs = runSh("use", "research")
	want(t, "use outside the prompt", code, exitUsage, errs, "use works only at the prompt")
	code, _, errs = runSh("start")
	want(t, "mekosh start", code, exitUsage, errs, "mekosh: start is a meko command: meko start\n")
}

// meko's usage commands are mekosh's now: meko names the command to run instead, and runs nothing.
func TestMekoNamesTheMekoshCommand(t *testing.T) {
	for _, name := range []string{"search", "add_memory", "datapacks", "create_datapack", "delete_datapack", "upload",
		"upload_status", "export"} {
		code, out, errs := run(name, "--data-dir", "/nonexistent", "two words")
		if code != exitUsage || out != "" ||
			errs != "meko: "+name+" is a mekosh command now: mekosh "+name+" --data-dir /nonexistent 'two words'\n" {
			t.Errorf("meko %s: exit %d, output %q, errors %q", name, code, out, errs)
		}
	}
}

func TestMainPicksTheProgramByName(t *testing.T) {
	var out, errb bytes.Buffer
	Main("/home/me/.local/bin/mekosh", []string{"--help"}, &out, &errb)
	want(t, "as mekosh", 0, 0, out.String(), "Usage: mekosh")
	out.Reset()
	Main("/opt/meko/bin/meko", []string{"--help"}, &out, &errb)
	want(t, "as meko", 0, 0, out.String(), "Usage: meko <command>")
}

func TestSplitWords(t *testing.T) {
	for line, words := range map[string][]string{
		"search \"how do releases work\" --limit 3\n": {"search", "how do releases work", "--limit", "3"},
		`add_memory 'it''s' "a \"b\" \$c \d"`:         {"add_memory", "its", `a "b" $c \d`},
		`upload my\ notes.md`:                         {"upload", "my notes.md"},
		"   \t\n":                                     nil,
		"# a comment":                                 nil,
		"datapacks # and a comment":                   {"datapacks"},
		`search a#b ""`:                               {"search", "a#b", ""},
	} {
		got, err := splitWords(line)
		if err != nil || !reflect.DeepEqual(got, words) {
			t.Errorf("splitWords(%q) = %q, %v; want %q", line, got, err, words)
		}
	}
	if _, err := splitWords(`search "unclosed`); err == nil || !strings.Contains(err.Error(), `unclosed " quote`) {
		t.Errorf("an unclosed quote: %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote([]string{"mekosh", "search", "it's here", "--limit", "3", ""}); got !=
		`mekosh search 'it'\''s here' --limit 3 ''` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestPromptArgs(t *testing.T) {
	for _, c := range []struct {
		words             []string
		datapack, dataDir string
		want              []string
	}{
		{[]string{"search", "q"}, "research", "", []string{"search", "q", "--datapack", "research"}},
		{[]string{"search", "q", "--datapack=other"}, "research", "", []string{"search", "q", "--datapack=other"}},
		{[]string{"search", "q", "-datapack", "other"}, "research", "", []string{"search", "q", "-datapack", "other"}},
		{[]string{"datapacks"}, "research", "/d", []string{"datapacks", "--data-dir", "/d"}},
		{[]string{"add_memory", "x"}, "", "", []string{"add_memory", "x"}},
	} {
		if got := promptArgs(c.words, c.datapack, c.dataDir); !reflect.DeepEqual(got, c.want) {
			t.Errorf("promptArgs(%q, %q, %q) = %q, want %q", c.words, c.datapack, c.dataDir, got, c.want)
		}
	}
}

func TestReport(t *testing.T) {
	var b bytes.Buffer
	r := newReport(&b)
	if r.color {
		t.Error("color on a buffer")
	}
	r.step(stepWarn, "Memory engine", "starting")
	r.box("Meko 1.0 is running", [][2]string{{"Agents (MCP, API)", "http://127.0.0.1:7778/mcp"}, {"Data", "/d"}})
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if lines[0] != "  ! Memory engine   starting" || lines[1] != "" {
		t.Errorf("step %q, then %q", lines[0], lines[1])
	}
	width := utf8.RuneCountInString(lines[2])
	for _, l := range lines[2:] {
		if utf8.RuneCountInString(l) != width {
			t.Errorf("box lines differ in width:\n%s", b.String())
			break
		}
	}
	if !strings.Contains(b.String(), "│ Data                /d                        │") {
		t.Errorf("box rows aren't aligned:\n%s", b.String())
	}
	t.Setenv("NO_COLOR", "1")
	if newReport(os.Stdout).color {
		t.Error("color with NO_COLOR")
	}
}

// An install from before mekosh gets ~/.local/bin/mekosh at serve's start, beside its meko; one
// whose meko isn't there, or that has a mekosh already, is left alone.
func TestLinkMekosh(t *testing.T) {
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
	mekosh := filepath.Join(home, ".local", "bin", "mekosh")
	if err := linkMekosh(app, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(mekosh); !os.IsNotExist(err) {
		t.Fatalf("mekosh without a meko command: %v", err)
	}
	if _, err := app.LinkCommand(home); err != nil {
		t.Fatal(err)
	}
	if err := linkMekosh(app, home); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(mekosh); err != nil || target != app.CurrentExe() {
		t.Fatalf("mekosh -> %q, %v", target, err)
	}
	if err := app.UnlinkCommandAs(home, "mekosh"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mekosh, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linkMekosh(app, home); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(mekosh); string(b) != "mine" {
		t.Errorf("a mekosh that was there was replaced")
	}
}
