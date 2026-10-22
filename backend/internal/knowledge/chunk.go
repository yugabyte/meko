package knowledge

import (
	"strings"
	"unicode/utf8"
)

// Chunk sizes, in characters, as Meko Cloud's splitter's: windows of ChunkSize overlapping by
// ChunkOverlap. ChunkSize also fits the embedding model, all-MiniLM-L6-v2, which reads 256 tokens
// (about 1000 characters) and would embed only the start of anything longer.
const (
	ChunkSize    = 1000
	ChunkOverlap = 100
	// minBody is the least room a section's prefix leaves for its text.
	minBody = 200
)

// separators are where the recursive splitter cuts, coarsest first; "" cuts between characters.
var separators = []string{"\n\n", "\n", " ", ""}

// splitText cuts text into chunks of at most size characters, cutting at paragraphs, then lines,
// then words, then characters, and packing the pieces so each chunk starts with up to overlap
// characters of the one before. Chunks are trimmed, and blank ones dropped.
func splitText(text string, size, overlap int) []string {
	return splitWith(text, separators, size, overlap)
}

func splitWith(text string, seps []string, size, overlap int) []string {
	sep, rest := seps[len(seps)-1], []string(nil)
	for i, s := range seps {
		if s == "" || strings.Contains(text, s) {
			sep, rest = s, seps[i+1:]
			break
		}
	}
	var pieces []string
	if sep == "" {
		pieces = strings.Split(text, "")
	} else {
		pieces = strings.Split(text, sep)
	}
	var out, fit []string
	for _, p := range pieces {
		if runes(p) <= size {
			fit = append(fit, p)
			continue
		}
		out = append(out, merge(fit, sep, size, overlap)...)
		fit = nil
		if len(rest) == 0 {
			out = append(out, p)
		} else {
			out = append(out, splitWith(p, rest, size, overlap)...)
		}
	}
	return append(out, merge(fit, sep, size, overlap)...)
}

// merge packs pieces joined by sep into chunks of at most size characters, each starting with the
// last pieces of the one before that fit in overlap.
func merge(pieces []string, sep string, size, overlap int) []string {
	sepLen := runes(sep)
	var out, cur []string
	total := 0
	emit := func() {
		if c := strings.TrimSpace(strings.Join(cur, sep)); c != "" {
			out = append(out, c)
		}
	}
	joined := func(n int) int { // what adding a piece of n characters costs
		if len(cur) > 0 {
			return n + sepLen
		}
		return n
	}
	for _, p := range pieces {
		n := runes(p)
		if total+joined(n) > size && len(cur) > 0 {
			emit()
			for total > 0 && (total > overlap || total+joined(n) > size) {
				total -= runes(cur[0])
				if len(cur) > 1 {
					total -= sepLen
				}
				cur = cur[1:]
			}
		}
		total += joined(n)
		cur = append(cur, p)
	}
	emit()
	return out
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// packLines packs a CSV's lines into chunks of at most ChunkSize characters overlapping by
// ChunkOverlap, as Meko Cloud's line splitter does (the header isn't repeated). A line longer than
// a chunk is split like text.
func packLines(text string) []string {
	var out, fit []string
	flush := func() {
		out = append(out, merge(fit, "\n", ChunkSize, ChunkOverlap)...)
		fit = nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if runes(line) <= ChunkSize {
			fit = append(fit, line)
			continue
		}
		flush()
		out = append(out, splitText(line, ChunkSize, ChunkOverlap)...)
	}
	flush()
	return out
}

// section is a run of a document's text under one heading path ("" for the text before the first
// heading).
type section struct {
	path []string
	text string
}

// sectionChunks chunks a document's sections, prefixing each chunk with the document's title and
// its section's heading path, as Meko Cloud's Markdown chunker does: "Document: <title>" and
// "Section: h1 > h2". The prefix counts toward the chunk's size and is repeated on every chunk of a
// section that's cut.
func sectionChunks(title string, sections []section) []string {
	var out []string
	for _, s := range sections {
		body := strings.TrimSpace(s.text)
		if body == "" {
			continue
		}
		prefix := ""
		if title != "" {
			prefix = "Document: " + title + "\n"
		}
		if len(s.path) > 0 {
			prefix += "Section: " + strings.Join(s.path, " > ") + "\n"
		}
		if prefix != "" {
			prefix += "\n"
		}
		room := max(ChunkSize-runes(prefix), minBody)
		for _, c := range splitText(body, room, min(ChunkOverlap, room/4)) {
			out = append(out, prefix+c)
		}
	}
	return out
}

// headings tracks the heading path while walking a document's headings.
type headings struct{ levels [6]string }

// enter records a heading of level 1 to 6 and returns the path down to it.
func (h *headings) enter(level int, text string) []string {
	level = min(max(level, 1), 6)
	h.levels[level-1] = text
	for i := level; i < 6; i++ {
		h.levels[i] = ""
	}
	var path []string
	for _, t := range h.levels[:level] {
		if t != "" {
			path = append(path, t)
		}
	}
	return path
}
