package jepsen

// Hotpatches for the jepsen checkout.
//
// jepsen is written against YugabyteDB's PostgreSQL fork; an amp compute is
// stock PostgreSQL (18.2, compute/vendor/revisions.json). Most of the suite does
// not notice — the DDL, the CONCAT-based append, the covering indexes, the
// isolation levels and the YugabyteDB JDBC driver itself were all verified to
// work unchanged against PostgreSQL 18 — but a few lines reach for something
// only YugabyteDB has. A patch here rewrites one of those lines in the checkout
// before the first run.
//
// Patching the checkout rather than forking yugabyte/jepsen keeps the upstream
// branch the source of truth. The cost is that an anchor can vanish under us,
// which is why a Find that matches nothing is a hard ERROR by default: with
// repo_ref tracking a branch, a silently skipped patch would mean the run does
// not test what its config says it tests.

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// Patch is one literal (not regex) substitution inside the jepsen checkout.
type Patch struct {
	// File is relative to <repo>/yugabyte.
	File string
	// Find is matched literally; every occurrence is replaced.
	Find    string
	Replace string
	// Reason is recorded on the step and must say what breaks without it.
	Reason string
	// Optional downgrades "anchor not found" from an error to a recorded skip,
	// for a patch that only applies to some upstream revisions.
	Optional bool
}

// PatchResult reports what one patch did, for the step properties.
type PatchResult struct {
	File         string
	Reason       string
	Replacements int
	Skipped      bool
	SkipReason   string
}

// DefaultPatches is the set applied when apps.jepsen.patches is unset.
//
// Only ONE entry today, and it is defensive rather than corrective: the
// follower-read GUC below is already gated on the cluster version, and jepsen's
// --version defaults to 1.3.1.0 while the gate needs 2.8.0.0-b1, so the line
// does not currently execute. That leaves the suite depending on an unrelated
// default staying where it is. Pinning the flag off makes it depend on nothing.
func DefaultPatches() []Patch {
	return []Patch{{
		File:    "src/yugabyte/ysql/bank_improved.clj",
		Find:    "(def enable-follower-reads true)",
		Replace: "(def enable-follower-reads false)",
		Reason: "bank-contention issues SET yb_read_from_followers on every read; that GUC is " +
			"YugabyteDB's and stock PostgreSQL answers 'unrecognized configuration parameter'. " +
			"It is reached only when the cluster version clears 2.8.0.0-b1, which jepsen's " +
			"--version default does not — so this pins off a branch that is currently " +
			"unreachable only by accident.",
	}}
}

// PatchesFromConfig reads apps.jepsen.patches; unset → DefaultPatches. An entry
// missing file or find is dropped, because the two of them are what a patch IS.
func PatchesFromConfig(get Getter) []Patch {
	raw := get.GetMapSlice("apps.jepsen.patches", nil)
	if raw == nil {
		return DefaultPatches()
	}
	out := make([]Patch, 0, len(raw))
	for _, m := range raw {
		p := Patch{
			File:     mapString(m, "file"),
			Find:     mapString(m, "find"),
			Replace:  mapString(m, "replace"),
			Reason:   mapString(m, "reason"),
			Optional: mapBool(m, "optional"),
		}
		if p.File == "" || p.Find == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func mapString(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func mapBool(m map[string]any, k string) bool {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// ApplyPatches rewrites the checkout under repoDir. It reads each file whole,
// substitutes in Go (so nothing in the payload has to survive a shell), and
// writes it back base64-encoded — base64 is the one encoding needing no quoting
// at all, and a clojure source file is full of quotes and parens.
func (a *App) ApplyPatches(ctx context.Context, repoDir string, patches []Patch) ([]PatchResult, error) {
	if repoDir == "" {
		repoDir = DefaultRepoDir
	}
	results := make([]PatchResult, 0, len(patches))
	for _, p := range patches {
		res, err := a.applyPatch(ctx, repoDir+"/yugabyte/"+p.File, p)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

func (a *App) applyPatch(ctx context.Context, path string, p Patch) (PatchResult, error) {
	out := PatchResult{File: p.File, Reason: p.Reason}

	read, err := a.ex.Run(ctx, fmt.Sprintf("cat %s", shellQuote(path)))
	if err != nil {
		return out, fmt.Errorf("jepsen patch %s: read: %w", p.File, err)
	}
	if read.ExitCode != 0 {
		if p.Optional {
			out.Skipped, out.SkipReason = true, "file not present in this revision"
			return out, nil
		}
		return out, fmt.Errorf("jepsen patch %s: file not found in the checkout (%s)",
			p.File, strings.TrimSpace(read.Stderr))
	}
	// A truncated read would be written back as the WHOLE file and silently
	// destroy the rest of it. Refuse rather than corrupt the checkout.
	if read.Truncated {
		return out, fmt.Errorf("jepsen patch %s: the file did not fit the exec capture buffer, "+
			"so patching it would truncate the source", p.File)
	}

	n := strings.Count(read.Stdout, p.Find)
	if n == 0 {
		if p.Optional {
			out.Skipped, out.SkipReason = true, "anchor not present in this revision"
			return out, nil
		}
		return out, fmt.Errorf("jepsen patch %s: the anchor %q is not in the checkout, so the "+
			"upstream revision has moved and this run would not be testing what it claims "+
			"(patch reason: %s)", p.File, p.Find, p.Reason)
	}

	patched := strings.ReplaceAll(read.Stdout, p.Find, p.Replace)
	enc := base64.StdEncoding.EncodeToString([]byte(patched))
	if err := a.runOK(ctx, fmt.Sprintf("printf %%s %s | base64 -d > %s",
		shellQuote(enc), shellQuote(path))); err != nil {
		return out, fmt.Errorf("jepsen patch %s: write back: %w", p.File, err)
	}

	// Read back: a write that reported success but left the anchor in place
	// would be the same silent no-op the hard error above exists to prevent.
	verify, verr := a.ex.Run(ctx, fmt.Sprintf("grep -cF -- %s %s || true",
		shellQuote(p.Find), shellQuote(path)))
	if verr != nil {
		return out, fmt.Errorf("jepsen patch %s: read back: %w", p.File, verr)
	}
	if got := strings.TrimSpace(verify.Stdout); got != "0" {
		return out, fmt.Errorf("jepsen patch %s: the anchor is still present after the write, or the read-back failed (grep -c said %q)",
			p.File, got)
	}
	out.Replacements = n
	return out, nil
}
