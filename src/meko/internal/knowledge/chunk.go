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
	// maxHeading caps a title or a heading in a chunk's prefix, in characters, so a huge one can't
	// make every chunk of the file huge.
	maxHeading = 200
)

// shorten cuts s to at most n characters, marking the cut.
func shorten(s string, n int) string {
	if len(s) <= n { // n bytes are at most n characters
		return s
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// separators are where the recursive splitter cuts, coarsest first; "" cuts between characters.
var separators = []string{"\n\n", "\n", " ", ""}

// splitText cuts text into chunks of at most size characters, cutting at paragraphs, then lines,
// then words, then characters, and packing the pieces so each chunk starts with up to overlap
// characters of the one before. Chunks are trimmed, and blank ones dropped.
func splitText(text string, size, overlap int) []string {
	return splitWith(text, separators, size, overlap)
}

// splitWith cuts text at the first of seps it contains, packs the pieces that fit into chunks,
// and cuts each piece longer than a chunk at the separators after that one. With no separator in
// text, or "" reached, it cuts between characters (cutRunes). Pieces are found as it goes and held
// by position, so no level makes a string per piece: text made of millions of tiny pieces took
// gigabytes that way.
func splitWith(text string, seps []string, size, overlap int) []string {
	for i, sep := range seps {
		if sep == "" {
			break
		}
		if !strings.Contains(text, sep) {
			continue
		}
		rest := seps[i+1:]
		p := newPacker(text, sep, size, overlap)
		eachPiece(text, sep, func(start, end int) {
			if n := runes(text[start:end]); n <= size {
				p.add(start, end, n)
				return
			}
			p.flush()
			p.out = append(p.out, splitWith(text[start:end], rest, size, overlap)...)
		})
		p.flush()
		return p.out
	}
	return cutRunes(text, size, overlap)
}

// eachPiece calls f with the start and end of each piece of text between seps, the pieces
// strings.Split would make, without making them.
func eachPiece(text, sep string, f func(start, end int)) {
	for start := 0; ; {
		i := strings.Index(text[start:], sep)
		if i < 0 {
			f(start, len(text))
			return
		}
		f(start, start+i)
		start += i + len(sep)
	}
}

// cutRunes cuts text that has no separator into windows of size characters, each starting with
// the last overlap characters of the one before (at most size-1), as packing it one character at a
// time would. It slices the windows out of text instead of making a string per character, which
// took gigabytes on a large file with no spaces or newlines. Windows are trimmed after they're
// cut, so a chunk can lose some of its overlap, and blank ones are dropped. size must be at least
// 1: a smaller one is taken as 1, where the old splitter kept every character, blank ones too.
func cutRunes(text string, size, overlap int) []string {
	size = max(size, 1)
	step := size - min(max(overlap, 0), size-1) // characters from one window's start to the next's
	var out []string
	for start := 0; ; {
		end, next := start, start
		for i := 0; i < size && end < len(text); i++ {
			_, w := utf8.DecodeRuneInString(text[end:])
			end += w
			if i+1 == step {
				next = end
			}
		}
		if c := strings.TrimSpace(text[start:end]); c != "" {
			out = append(out, c)
		}
		if end == len(text) { // this window reached the end: the next would only repeat its tail
			return out
		}
		start = next
	}
}

// packer packs pieces of text joined by sep into chunks of at most size characters, each starting
// with the last pieces of the one before that fit in overlap. A piece is held as its place in
// text, and a chunk is sliced out of text, so packing makes no string per piece and no copy.
// Chunks are trimmed, and blank ones dropped.
type packer struct {
	text, sep     string
	sepLen        int // sep's length in characters
	size, overlap int
	cur           []span // the pieces of the chunk being filled, consecutive in text
	total         int    // the chunk's length in characters, separators included
	out           []string
}

// span is a piece of text: where it starts and ends, in bytes, and its length in characters.
type span struct{ start, end, n int }

func newPacker(text, sep string, size, overlap int) *packer {
	return &packer{text: text, sep: sep, sepLen: runes(sep), size: size, overlap: overlap}
}

// joinCost is what joining a piece to a chunk of pieces costs: the separator, unless it's the first.
func (p *packer) joinCost(pieces int) int {
	if pieces > 0 {
		return p.sepLen
	}
	return 0
}

// add adds text[start:end], of n characters. When it doesn't fit, the chunk is emitted and its
// first pieces dropped until what's left fits in overlap and leaves room for the new piece.
func (p *packer) add(start, end, n int) {
	if len(p.cur) > 0 && p.total+n+p.joinCost(len(p.cur)) > p.size {
		p.emit()
		k := 0 // pieces dropped
		for p.total > 0 && (p.total > p.overlap || p.total+n+p.joinCost(len(p.cur)-k) > p.size) {
			p.total -= p.cur[k].n
			if len(p.cur)-k > 1 {
				p.total -= p.sepLen
			}
			k++
		}
		p.cur = p.cur[:copy(p.cur, p.cur[k:])] // reuse the buffer, so it grows only to one chunk
	}
	p.total += n + p.joinCost(len(p.cur))
	p.cur = append(p.cur, span{start, end, n})
}

// flush emits the chunk being filled and starts packing afresh.
func (p *packer) flush() {
	p.emit()
	p.cur, p.total = p.cur[:0], 0
}

func (p *packer) emit() {
	if len(p.cur) == 0 {
		return
	}
	if c := strings.TrimSpace(p.chunk()); c != "" {
		p.out = append(p.out, c)
	}
}

// chunk is the pieces being packed, joined by sep. Pieces with just sep between them in text are
// a slice of it. A CSV line whose trailing \r was trimmed isn't, and those pieces are joined.
func (p *packer) chunk() string {
	for i := 1; i < len(p.cur); i++ {
		if p.cur[i-1].end+len(p.sep) != p.cur[i].start {
			var b strings.Builder
			for j, s := range p.cur {
				if j > 0 {
					b.WriteString(p.sep)
				}
				b.WriteString(p.text[s.start:s.end])
			}
			return b.String()
		}
	}
	return p.text[p.cur[0].start:p.cur[len(p.cur)-1].end]
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// packLines packs a CSV's lines into chunks of at most ChunkSize characters overlapping by
// ChunkOverlap, as Meko Cloud's line splitter does (the header isn't repeated). A line longer than
// a chunk is split like text.
func packLines(text string) []string {
	p := newPacker(text, "\n", ChunkSize, ChunkOverlap)
	eachPiece(text, "\n", func(start, end int) {
		line := strings.TrimRight(text[start:end], "\r")
		if n := runes(line); n <= ChunkSize {
			p.add(start, start+len(line), n)
			return
		}
		p.flush()
		p.out = append(p.out, splitText(line, ChunkSize, ChunkOverlap)...)
	})
	p.flush()
	return p.out
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
	title = shorten(title, maxHeading)
	for _, s := range sections {
		body := strings.TrimSpace(s.text)
		if body == "" {
			continue
		}
		prefix := ""
		if title != "" {
			prefix = "Document: " + title + "\n"
		}
		if len(s.path) > 0 { // each heading was cut as it was read (headings.enter)
			prefix += "Section: " + shorten(strings.Join(s.path, " > "), 2*maxHeading) + "\n"
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
	h.levels[level-1] = shorten(text, maxHeading)
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
