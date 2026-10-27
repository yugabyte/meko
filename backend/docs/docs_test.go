// Package docs holds the backend's design docs; its only code is the test that checks them.
package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The docs describe Meko as it is now, so they carry no delivery history: no status lines, no
// milestones, and no PR or issue numbers (backend/AGENTS.md). That history is the git log's, and
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
