// Package docs holds the backend's design docs; its only code is the test that checks them.
package docs

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The docs describe Meko as it is now, so they carry no delivery history: no status lines, no
// milestones, and no PR or issue numbers (src/meko/AGENTS.md). That history is the git log's, and
// it goes stale in a doc the moment the next change lands.
var history = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`(?i)^\s*status:`), "a status line"},
	{regexp.MustCompile(`(?i)\bmilestones?\b`), "a milestone"},
	{regexp.MustCompile(`(^|[^\w&/])#\d+\b`), "a PR or issue number"},
}

func TestDocsCarryNoHistory(t *testing.T) {
	files, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no docs found")
	}
	for _, name := range files {
		b, err := os.ReadFile(name) //nolint:gosec // G304: a doc in this folder, from the glob above
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, h := range history {
				if h.re.MatchString(line) {
					t.Errorf("%s:%d has %s: %q", name, i+1, h.what, line)
				}
			}
		}
	}
}

// The links a doc can make: inline, [text](target) or [text](<target with spaces>), either with an
// optional "title"; and a reference definition, [ref]: target, at the start of a line.
var (
	inlineLink    = regexp.MustCompile(`\]\((?:<([^>]*)>|([^)\s]*))(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*\)`)
	referenceLink = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:\s*(?:<([^>]*)>|(\S+))`)
)

// linkTargets is the target of every link on a line, as written.
func linkTargets(line string) []string {
	var targets []string
	add := func(m []string) {
		if m[1] != "" {
			targets = append(targets, m[1])
		} else {
			targets = append(targets, m[2])
		}
	}
	for _, m := range inlineLink.FindAllStringSubmatch(line, -1) {
		add(m)
	}
	if m := referenceLink.FindStringSubmatch(line); m != nil {
		add(m)
	}
	return targets
}

func TestLinkTargets(t *testing.T) {
	for _, c := range []struct {
		line string
		want []string
	}{
		{"see [a](a.md) and [b](b.md#part)", []string{"a.md", "b.md#part"}},
		{`[a](a.md "A title")`, []string{"a.md"}},
		{"[a](a.md 'A title')", []string{"a.md"}},
		{"[a](<a doc.md>)", []string{"a doc.md"}},
		{"[ref]: ref.md", []string{"ref.md"}},
		{`   [ref]: <ref doc.md> "A title"`, []string{"ref doc.md"}},
		{"[a][ref] uses a definition, checked where it's defined", nil},
		{"no link here (a.md)", nil},
	} {
		got := linkTargets(c.line)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("linkTargets(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// TestDocsLinksResolve checks that every relative link in the docs, inline or a reference
// definition, names a file or folder that exists, so a doc that's deleted or moved takes its links
// with it. Web links and links to a heading in the same doc aren't checked, nor is a link's heading
// in another doc.
func TestDocsLinksResolve(t *testing.T) {
	files, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		b, err := os.ReadFile(name) //nolint:gosec // G304: a doc in this folder, from the glob above
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, l := range linkTargets(line) {
				target, _, _ := strings.Cut(l, "#")
				if target == "" {
					continue
				}
				if u, err := url.Parse(target); err == nil && u.Scheme != "" {
					continue
				}
				if _, err := os.Stat(target); err != nil {
					t.Errorf("%s:%d links to %s, which doesn't exist", name, i+1, l)
				}
			}
		}
	}
}
