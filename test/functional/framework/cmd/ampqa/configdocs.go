package main

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"

	"github.com/yugabyte/yb-amp/test/functional/framework/configs"
)

// keyLineRE matches a YAML mapping key at the start of a line: leading spaces
// (the indent), the key, a colon, then an optional value/inline-comment. List
// items ("- ...") and continuation lines never match, so only real keys build a
// dotted path.
var keyLineRE = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+):(\s|$)`)

// parseConfigDocs parses the embedded configs/default.yaml into a
// dotted.key → description map, reading the description from the contiguous
// block of `#` comment lines immediately preceding each key. This is the SINGLE
// source of per-parameter documentation (the same `#` docs a human reads in the
// file) — `ampqa describe` resolves it rather than duplicating doc strings in Go.
//
// A blank line between a comment block and a key breaks the association (so the
// file's top-of-file header is not attributed to the first key). Multi-line
// comment blocks are joined with a single space; docFirstLine trims that back to
// a one-liner for compact display.
func parseConfigDocs() map[string]string {
	return parseCommentDocs(configs.DefaultYAML)
}

// parseCommentDocs is parseConfigDocs split out for unit-testing against a
// sample rather than the embedded file.
func parseCommentDocs(yaml []byte) map[string]string {
	docs := map[string]string{}
	var pending []string // contiguous comment lines gathered for the next key
	type frame struct {  // an ancestor key and the indent it lives at
		indent int
		key    string
	}
	var stack []frame

	sc := bufio.NewScanner(bytes.NewReader(yaml))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			pending = pending[:0] // blank line breaks comment→key association
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			pending = append(pending, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
			continue
		}

		m := keyLineRE.FindStringSubmatch(line)
		if m == nil {
			pending = pending[:0] // list items / multi-line scalars aren't keys
			continue
		}
		indent := len(m[1])
		key := m[2]

		// Pop ancestors at or deeper than this key's indent, then push it.
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, frame{indent: indent, key: key})

		if len(pending) > 0 {
			parts := make([]string, len(stack))
			for i, f := range stack {
				parts[i] = f.key
			}
			docs[strings.Join(parts, ".")] = strings.Join(pending, " ")
		}
		pending = pending[:0]
	}
	return docs
}

// docFirstLine returns the first sentence-ish line of a (possibly space-joined
// multi-line) comment, for compact one-line display. It cuts at the first
// sentence-ending period followed by a space, else returns the whole string.
func docFirstLine(doc string) string {
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}
