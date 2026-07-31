package main

import (
	"strings"
	"testing"
)

// TestParseCommentDocsSample: comments immediately preceding a key become that
// key's doc — for top-level, nested, and multi-line comment blocks — while a
// blank line between a comment block and a key breaks the association (the
// file header is not attributed to the first key).
func TestParseCommentDocsSample(t *testing.T) {
	sample := []byte(`# File header line one.
# File header line two.

# Owner of everything.
owner: "ampqa"

sched:
  # Total environments at once.
  capacity: 8
  # First line of a two-line comment.
  # Second line continues it.
  parallel: 1

# A list value with an inline comment.
ports: [1, 2, 3] # trailing comment ignored
`)
	docs := parseCommentDocs(sample)

	cases := map[string]string{
		"owner":          "Owner of everything.",
		"sched.capacity": "Total environments at once.",
		"sched.parallel": "First line of a two-line comment. Second line continues it.",
		"ports":          "A list value with an inline comment.",
	}
	for key, want := range cases {
		if got := docs[key]; got != want {
			t.Errorf("docs[%q] = %q, want %q", key, got, want)
		}
	}

	// The file header (before the blank line) must NOT attach to `owner`.
	if strings.Contains(docs["owner"], "File header") {
		t.Errorf("docs[owner] wrongly absorbed the file header: %q", docs["owner"])
	}
	// `sched` itself has no preceding comment (blank line above), so no doc.
	if got, ok := docs["sched"]; ok {
		t.Errorf("docs[sched] = %q, want absent (no preceding comment)", got)
	}
}

// TestParseConfigDocsEmbedded: the parser reads the REAL embedded default.yaml
// and maps a representative set of keys — top-level, nested, and deeply nested —
// to the doc from their preceding `#` comment.
func TestParseConfigDocsEmbedded(t *testing.T) {
	docs := parseConfigDocs()

	if got := docs["sched.capacity"]; got != "Total environments that may exist at once across the whole run." {
		t.Errorf("docs[sched.capacity] = %q", got)
	}
	if got := docs["owner"]; !strings.HasPrefix(got, "owner is stamped") || !strings.Contains(got, "-o owner=") {
		t.Errorf("docs[owner] = %q, want the full multi-line owner doc", got)
	}
	if got := docs["perf.pgbench.scale"]; !strings.Contains(got, "pgbench -s") {
		t.Errorf("docs[perf.pgbench.scale] = %q, want the pgbench -s doc", got)
	}
	if got := docs["chaos.delay_ms"]; got == "" {
		t.Errorf("docs[chaos.delay_ms] missing")
	}
}

// TestDocFirstLine cuts a joined multi-line comment back to its first sentence.
func TestDocFirstLine(t *testing.T) {
	if got := docFirstLine("First sentence. Second sentence."); got != "First sentence." {
		t.Errorf("docFirstLine = %q, want %q", got, "First sentence.")
	}
	if got := docFirstLine("No sentence break here"); got != "No sentence break here" {
		t.Errorf("docFirstLine = %q, want the whole string", got)
	}
}
