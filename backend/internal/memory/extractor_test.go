package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Claude adapter runs claude -p with no tools and the reply schema, the turns on stdin, and
// reads structured_output; an error reply (not signed in) is an error.
func TestClaudeHarness(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	stdinFile := filepath.Join(dir, "stdin")
	script := filepath.Join(dir, "claude")
	write := func(reply string) {
		t.Helper()
		body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat > " + stdinFile + "\necho '" + reply + "'\n"
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // G306: a script the test runs
			t.Fatal(err)
		}
	}
	write(`{"is_error":false,"result":"","structured_output":{"memory":[{"text":"The user likes tea.","turn":0}]}}`)
	h := claudeHarness{command: script, model: "haiku"}
	facts, err := h.Extract(context.Background(), "SYSTEM", "the turns")
	if err != nil || len(facts) != 1 || facts[0].Text != "The user likes tea." || facts[0].Turn != 0 {
		t.Fatalf("%v %v", facts, err)
	}
	args, _ := os.ReadFile(argsFile)
	in, _ := os.ReadFile(stdinFile)
	for _, want := range []string{"-p\n", "--tools\n\n", "--strict-mcp-config", "--setting-sources\nlocal",
		"--model\nhaiku", "--system-prompt\nSYSTEM", "--json-schema"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if string(in) != "the turns" {
		t.Errorf("stdin %q", in)
	}
	write(`{"is_error":true,"result":"Not logged in · Please run /login"}`)
	if _, err = h.Extract(context.Background(), "S", "x"); err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("an error reply: %v", err)
	}
	write(`not json`)
	if _, err = h.Extract(context.Background(), "S", "x"); err == nil {
		t.Error("a reply that isn't JSON passed")
	}
}

// clip keeps a long text's start and end and says how much it cut; trivial is a go-ahead.
func TestClipAndTrivial(t *testing.T) {
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip %q", got)
	}
	long := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	if got := clip(long, 40); !strings.HasPrefix(got, strings.Repeat("a", 30)) ||
		!strings.HasSuffix(got, strings.Repeat("z", 10)) || !strings.Contains(got, "160 characters cut") {
		t.Errorf("clip %q", got)
	}
	if !trivial("ok") || !trivial("  yes do it ") || trivial("We use pnpm, never npm.") {
		t.Error("trivial")
	}
}
