package jepsen_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/jepsen"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

const bankImprovedSrc = `(ns yugabyte.ysql.bank-improved)
(def enable-follower-reads true)
(def minimal-follower-read-version "2.8.0.0-b1")
`

// patchExec answers `cat` with a file body and records what was written back.
type patchExec struct {
	body      string
	truncated bool
	catExit   int
	written   string
	grepOut   string
	grepBlank bool // grep -c printed nothing: the read-back itself failed
	commands  []string
}

func (p *patchExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	p.commands = append(p.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "cat "):
		return exec.Result{ExitCode: p.catExit, Stdout: p.body, Truncated: p.truncated}, nil
	case strings.HasPrefix(cmd, "printf"):
		// The payload is base64 between the first and last single quote.
		i, j := strings.Index(cmd, "'"), strings.Index(cmd, "' | base64 -d")
		if i >= 0 && j > i {
			raw, err := base64.StdEncoding.DecodeString(cmd[i+1 : j])
			if err != nil {
				return exec.Result{ExitCode: 1}, nil
			}
			p.written = string(raw)
			p.body = p.written
		}
		return exec.Result{ExitCode: 0}, nil
	case strings.HasPrefix(cmd, "grep -cF"):
		out := p.grepOut
		if p.grepBlank {
			return exec.Result{ExitCode: 0}, nil
		}
		if out == "" {
			out = "0"
		}
		return exec.Result{ExitCode: 0, Stdout: out}, nil
	}
	return exec.Result{}, nil
}

func (p *patchExec) Copy(context.Context, string, string) error { return nil }

func TestApplyPatchesRewritesTheAnchor(t *testing.T) {
	ex := &patchExec{body: bankImprovedSrc}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	res, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches())
	if err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	if len(res) != 1 || res[0].Replacements != 1 || res[0].Skipped {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(ex.written, "(def enable-follower-reads false)") {
		t.Errorf("patched body did not flip the flag:\n%s", ex.written)
	}
	// The rest of the file must survive verbatim — a patch is a substitution,
	// not a rewrite.
	if !strings.Contains(ex.written, `(def minimal-follower-read-version "2.8.0.0-b1")`) {
		t.Errorf("patch dropped unrelated lines:\n%s", ex.written)
	}
}

// repo_ref tracks a BRANCH, so an anchor can vanish upstream. A silently
// skipped patch would leave the run claiming to test something it does not.
func TestApplyPatchesFailsOnAMissingAnchor(t *testing.T) {
	ex := &patchExec{body: "(ns yugabyte.ysql.bank-improved)\n"}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	_, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches())
	if err == nil {
		t.Fatal("a patch whose anchor is absent applied without error")
	}
	if !strings.Contains(err.Error(), "upstream revision has moved") {
		t.Errorf("error does not explain the cause: %v", err)
	}
	if ex.written != "" {
		t.Errorf("a failed patch still wrote the file: %q", ex.written)
	}
}

func TestApplyPatchesSkipsAnOptionalMissingAnchor(t *testing.T) {
	ex := &patchExec{body: "(ns x)\n"}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	res, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", []jepsen.Patch{{
		File: "src/x.clj", Find: "nope", Replace: "y", Reason: "r", Optional: true,
	}})
	if err != nil {
		t.Fatalf("optional patch errored: %v", err)
	}
	if len(res) != 1 || !res[0].Skipped {
		t.Fatalf("result = %+v", res)
	}
}

// Writing back a truncated read would silently destroy the rest of the source.
func TestApplyPatchesRefusesATruncatedRead(t *testing.T) {
	ex := &patchExec{body: bankImprovedSrc, truncated: true}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	_, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches())
	if err == nil || !strings.Contains(err.Error(), "truncate") {
		t.Fatalf("error = %v, want a truncation refusal", err)
	}
	if ex.written != "" {
		t.Errorf("a truncated read was written back: %q", ex.written)
	}
}

// A write that reports success but leaves the anchor is the same silent no-op
// the missing-anchor error exists to prevent.
func TestApplyPatchesFailsWhenTheAnchorSurvivesTheWrite(t *testing.T) {
	ex := &patchExec{body: bankImprovedSrc, grepOut: "1"}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	_, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches())
	if err == nil || !strings.Contains(err.Error(), "still present after the write") {
		t.Fatalf("error = %v, want a post-write verification failure", err)
	}
}

// An empty read-back is not a zero count: the `|| true` hides a grep that could
// not read the file at all.
func TestApplyPatchesFailsWhenTheReadBackSaysNothing(t *testing.T) {
	ex := &patchExec{body: bankImprovedSrc, grepBlank: true}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	_, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches())
	if err == nil || !strings.Contains(err.Error(), "read-back failed") {
		t.Fatalf("error = %v, want a read-back failure", err)
	}
}

func TestApplyPatchesFailsOnAMissingFile(t *testing.T) {
	ex := &patchExec{catExit: 1}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))

	_, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches())
	if err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("error = %v, want a missing-file failure", err)
	}
}

// The payload is a clojure source file full of quotes and parens; base64 is
// what keeps it out of the shell's reach entirely.
func TestApplyPatchesSendsThePayloadBase64Encoded(t *testing.T) {
	ex := &patchExec{body: bankImprovedSrc}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZBank, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))
	if _, err := app.ApplyPatches(context.Background(), "/opt/ampqa/jepsen", jepsen.DefaultPatches()); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	var write string
	for _, c := range ex.commands {
		if strings.HasPrefix(c, "printf") {
			write = c
		}
	}
	if !strings.Contains(write, "base64 -d") {
		t.Errorf("write command is not base64-encoded: %s", write)
	}
	if strings.Contains(write, "(def enable-follower-reads") {
		t.Errorf("the clojure payload reached the shell verbatim: %s", write)
	}
}

// --- config ---

type patchGetter struct {
	mapGetter
	patches []map[string]any
	set     bool
}

func (p patchGetter) GetMapSlice(string, []map[string]any) []map[string]any {
	if !p.set {
		return nil
	}
	return p.patches
}

func TestPatchesFromConfigDefaultsToTheBuiltInSet(t *testing.T) {
	got := jepsen.PatchesFromConfig(patchGetter{})
	if len(got) != len(jepsen.DefaultPatches()) {
		t.Errorf("unset patches = %d, want the built-in set", len(got))
	}
}

// An explicit empty list means "apply none" and must not silently fall back to
// the built-in set.
func TestPatchesFromConfigHonoursAnEmptyList(t *testing.T) {
	got := jepsen.PatchesFromConfig(patchGetter{set: true, patches: []map[string]any{}})
	if len(got) != 0 {
		t.Errorf("explicit [] = %d patches, want 0", len(got))
	}
}

func TestPatchesFromConfigReadsEntries(t *testing.T) {
	got := jepsen.PatchesFromConfig(patchGetter{set: true, patches: []map[string]any{
		{"file": "src/a.clj", "find": "x", "replace": "y", "reason": "r", "optional": true},
		{"find": "no file"},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d patches, want 1 (the entry with no file is not a patch)", len(got))
	}
	if got[0].File != "src/a.clj" || got[0].Find != "x" || got[0].Replace != "y" || !got[0].Optional {
		t.Errorf("parsed = %+v", got[0])
	}
}
