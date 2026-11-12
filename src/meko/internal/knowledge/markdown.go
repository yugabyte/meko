package knowledge

import (
	"bytes"
	"context"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// headingWindow is how much Markdown goldmark parses at a time. goldmark holds a syntax tree for
// all it's given (1.6 GB for a 50 MiB list), so a file is parsed a window at a time, and only a
// block that is followed by another block in its window is trusted.
const headingWindow = 1 << 20

// maxNesting is how many quotes and list items a line may open before the rest of its markers are
// read as text. goldmark takes time quadratic in a line's nesting depth (128 KiB of ">" took
// 3.8 s), and nothing nested in a quote or list item is a top-level heading.
const maxNesting = 32

// mdParser finds CommonMark's blocks only: no inline parsing, no tables and no paragraph
// transformers. Inline parsing and tables never move a top-level heading. Without the
// link-reference transformer, a paragraph of only link definitions over a setext underline
// ("[a]: /url" then "===") is a heading, where CommonMark reads definitions and a paragraph "==="
// (accepted: definitions are rare outside documents' ends). It's safe for concurrent use.
var mdParser = parser.NewParser(
	parser.WithBlockParsers(parser.DefaultBlockParsers()...),
	parser.WithInlineParsers(),
)

var (
	titleLine = regexp.MustCompile(`^title:\s*(.+?)\s*$`)
	// yamlLine is a line front matter may hold: a key, a list item, an indented continuation or a
	// comment.
	yamlLine = regexp.MustCompile(`^(?:[^\s#:][^:]*:(?:[ \t].*)?|[ \t]*-(?:[ \t].*)?|[ \t]+\S.*|[ \t]*#.*)$`)
)

// heading is a top-level heading: its level and text, and the bytes of its lines in the text
// ([start, end), the setext underline included).
type heading struct {
	start, end, level int
	text              string
	setext            bool
}

// markdownSections splits Markdown into sections at its top-level headings, ATX ("# x") and setext
// (a paragraph underlined with = or -), as CommonMark reads them (but for link definitions
// underlined as a setext heading: mdParser): none inside code, HTML blocks, quotes or list items.
// Each section's text is the raw Markdown between headings. The title is front matter's title,
// else "". It stops with ctx's error when ctx ends.
func markdownSections(ctx context.Context, text string) (title string, sections []section, err error) {
	title, text = frontMatter(text)
	hs, err := findHeadings(ctx, text, headingWindow)
	if err != nil {
		return "", nil, err
	}
	var h headings
	cur, prev := section{}, 0
	for _, x := range hs {
		cur.text = text[prev:x.start]
		sections = append(sections, cur)
		cur, prev = section{path: h.enter(x.level, x.text)}, x.end
	}
	cur.text = text[prev:]
	return title, append(sections, cur), nil
}

// frontMatter takes a leading "---" block that reads as YAML off text, closed by "---" or "...",
// and returns its title. Anything else, such as a "---" rule over prose, is left as Markdown.
func frontMatter(text string) (title, rest string) {
	first, body, ok := strings.Cut(text, "\n")
	if !ok || strings.TrimSpace(first) != "---" {
		return "", text
	}
	for off := 0; off < len(body); {
		line, next := body[off:], len(body)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], off+i+1
		}
		switch t := strings.TrimSpace(line); {
		case t == "---" || t == "...":
			for _, l := range strings.Split(body[:off], "\n") {
				if m := titleLine.FindStringSubmatch(l); m != nil {
					title = strings.Trim(m[1], `"'`)
				}
			}
			return title, body[next:]
		case t != "" && !yamlLine.MatchString(line):
			return "", text
		}
		off = next
	}
	return "", text
}

// findHeadings returns text's top-level headings, parsing window bytes at a time. Each window is
// cut at a line end and parsed alone; its blocks are trusted up to the last one that a parse can
// restart at, and the next window starts there. A window one block fills is restarted inside that
// block: at its last list item or quoted block, or a line of its paragraph; for a code or HTML
// block, the next window starts at the window's end, after the block's opening line again, which is
// all goldmark needs to read on as that block. Otherwise the window doubles. It checks ctx before
// each parse.
func findHeadings(ctx context.Context, text string, window int) ([]heading, error) {
	var hs []heading
	para := -1  // where the paragraph the last window was restarted inside starts, else -1
	carry := "" // the opening line of the code or HTML block the last window ended inside
	for start := 0; start < len(text); {
		for w := window; ; w *= 2 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end, last := len(text), true
			if start+w < len(text) {
				end, last = start+w, false
				if i := strings.LastIndexByte(text[start:end], '\n'); i >= 0 {
					end = start + i + 1
				}
			}
			src := make([]byte, 0, len(carry)+end-start)
			src = append(append(src, carry...), text[start:end]...)
			base := start - len(carry) // text's offset of src[0], as if carry were there
			limitNesting(src)
			var kids []ast.Node
			for k := mdParser.Parse(gmtext.NewReader(src)).FirstChild(); k != nil; k = k.NextSibling() {
				kids = append(kids, k)
			}
			keep, next, nextPara, nextCarry := len(kids), end, -1, ""
			if !last {
				n := len(kids)
				r := restartAt(kids, src)
				switch {
				case n > 0 && isLeaf(kids[n-1]) && src[len(src)-1] == '\n':
					if !openAtEnd(src, n) { // it ended on the window's last line
						break
					}
					op := carry
					if n > 1 || op == "" {
						op = openingLine(kids[n-1], src)
					}
					if op == "" || len(op) > window/2 {
						continue
					}
					keep, nextCarry = n-1, op
				case r > 0:
					keep, next = r, base+blockStart(kids[r], src)
				case n != 1:
					continue
				default:
					off, inPara := restartInside(kids[0], src)
					if off <= 0 {
						continue
					}
					keep, next = 0, base+off
					if inPara {
						nextPara = base + blockStart(kids[0], src)
						if para >= 0 && nextPara == start {
							nextPara = para // still the same paragraph
						}
					}
				}
			}
			for i, k := range kids[:keep] {
				h, ok := k.(*ast.Heading)
				if !ok || h.Lines().Len() == 0 { // an empty "#" has no text and changes no path
					continue
				}
				x := toHeading(h, src, base)
				if i == 0 && para >= 0 && x.start == start && x.setext {
					// The setext heading underlines the whole paragraph the last window ended in.
					x.start, x.text = para, joinLines(text[para:x.textEnd(text)])
				}
				hs = append(hs, x)
			}
			start, para, carry = next, nextPara, nextCarry
			break
		}
	}
	return hs, nil
}

// isLeaf says whether a block is code or HTML: one whose end its opening line alone decides.
func isLeaf(n ast.Node) bool {
	switch n.(type) {
	case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.HTMLBlock:
		return true
	}
	return false
}

// openAtEnd says whether the last of src's n blocks is still open after src's last line: parsed
// with a line of plain text after it, it takes that line in instead of ending before it.
func openAtEnd(src []byte, n int) bool {
	probe := make([]byte, 0, len(src)+2)
	probe = append(append(probe, src...), "x\n"...)
	for k := mdParser.Parse(gmtext.NewReader(probe)).FirstChild(); k != nil; k = k.NextSibling() {
		n--
	}
	return n == 0
}

// openingLine is the line, with its '\n', that opens a code or HTML block: its first line, or for a
// fenced code block the fence before it. It's "" when that can't be told.
func openingLine(b ast.Node, src []byte) string {
	s := blockStart(b, src)
	if _, ok := b.(*ast.FencedCodeBlock); ok {
		if lines := b.Lines(); lines.Len() > 0 {
			first := lineStart(src, lines.At(0).Start)
			if first == 0 {
				return ""
			}
			s = lineStart(src, first-1)
		} else {
			s = lineStart(src, len(src)-1) // nothing follows its fence yet
		}
		if t := bytes.TrimLeft(src[s:], " "); !bytes.HasPrefix(t, []byte("```")) &&
			!bytes.HasPrefix(t, []byte("~~~")) {
			return ""
		}
	}
	if s < 0 {
		return ""
	}
	return string(src[s:lineEnd(src, s)])
}

// restartAt is the last of kids, after the first, that a parse can start at: a heading, paragraph,
// list, quote or HTML block whose first line holds its opening (a fenced code block's lines start
// after its fence, so it never is). It's -1 when none can.
func restartAt(kids []ast.Node, src []byte) int {
	for i := len(kids) - 1; i >= 1; i-- {
		switch kids[i].(type) {
		case *ast.Heading, *ast.Paragraph, *ast.List, *ast.Blockquote, *ast.HTMLBlock:
			if blockStart(kids[i], src) > 0 && opensOnFirstLine(kids[i], src) {
				return i
			}
		}
	}
	return -1
}

// restartInside is where a parse can start inside a block that fills its window: the last of its
// list items or quoted blocks, after the first, that opens on its first line; in a paragraph, its
// last line, after the first, that can only be paragraph text (paragraphOnly); in a quote of one
// paragraph, such a line after "> ". It's 0 when there's none.
func restartInside(b ast.Node, src []byte) (off int, inPara bool) {
	switch b.(type) {
	case *ast.List, *ast.Blockquote:
		for c := b.LastChild(); c != nil && c != b.FirstChild(); c = c.PreviousSibling() {
			if s := blockStart(c, src); s > 0 && opensOnFirstLine(c, src) {
				return s, false
			}
		}
	case *ast.Paragraph:
		lines := b.Lines()
		for i := lines.Len() - 1; i >= 1; i-- {
			if s := lineStart(src, lines.At(i).Start); s > 0 && paragraphOnly(src[s:lineEnd(src, s)]) {
				return s, true
			}
		}
	}
	if p, ok := b.LastChild().(*ast.Paragraph); ok && b.Kind() == ast.KindBlockquote && b.ChildCount() == 1 {
		// One quoted paragraph: start at one of its lines that is "> " and paragraph text.
		lines := p.Lines()
		for i := lines.Len() - 1; i >= 1; i-- {
			s := lineStart(src, lines.At(i).Start)
			line := src[s:lineEnd(src, s)]
			if t := bytes.TrimLeft(line, " "); s > 0 && len(line)-len(t) < 4 && len(t) > 1 && t[0] == '>' &&
				paragraphOnly(bytes.TrimPrefix(t[1:], []byte(" "))) {
				return s, false
			}
		}
	}
	return 0, false
}

// paragraphOnly says whether a line can only be paragraph text, wherever a parse starts: at most 3
// spaces, then nothing that opens another block. A line may start with '#' (not 1-6 of them and a
// space: "#tag"), '*', '-' or '+' (not a list marker or a rule: "*draft*"), '=' or '-' (not only
// those: "=x"), or '`' or '~' (fewer than three: "“x"), but not '>', '<' or a tab, nor a list
// marker or a thematic break.
func paragraphOnly(line []byte) bool {
	line = bytes.TrimRight(line, "\n")
	text := bytes.TrimLeft(line, " ")
	if len(line)-len(text) > 3 || len(bytes.TrimSpace(text)) == 0 || listMarker(text) > 0 || isRule(text) {
		return false
	}
	switch c := text[0]; c {
	case '\t', '>', '<':
		return false
	case '#':
		n := leading(text, c)
		return n > 6 || (n < len(text) && text[n] != ' ' && text[n] != '\t')
	case '=', '-':
		return len(bytes.Trim(text, string(c)+" \t")) > 0 // not a setext underline
	case '`', '~':
		return leading(text, c) < 3
	}
	return true
}

// leading is how many times c starts b.
func leading(b []byte, c byte) int {
	n := 0
	for n < len(b) && b[n] == c {
		n++
	}
	return n
}

// blockStart is the start of the line holding a block's first line, or -1 when that can't be known
// (a block with no lines, or one whose lines start inside a fenced code block).
func blockStart(n ast.Node, src []byte) int {
	for ; n != nil && n.Type() == ast.TypeBlock; n = n.FirstChild() {
		if _, ok := n.(*ast.FencedCodeBlock); ok {
			return -1
		}
		if n.Lines().Len() > 0 {
			return lineStart(src, n.Lines().At(0).Start)
		}
	}
	return -1
}

// opensOnFirstLine says whether a parse started at a block's first line (by blockStart) opens it
// there: whether that line, before its text, holds a '>' or list marker for each quote and list
// item the block is in or opens. A list item whose marker is on a line of its own, or a quote whose
// first line is blank, opens a line earlier, and a parse started at blockStart would read it as
// something else.
func opensOnFirstLine(n ast.Node, src []byte) bool {
	s := blockStart(n, src)
	if s < 0 {
		return false
	}
	want := 0
	for p := n; p != nil && p.Kind() != ast.KindDocument; p = p.Parent() {
		want += containers(p)
	}
	leaf := n // the first block down from n that has lines
	for leaf != nil && leaf.Type() == ast.TypeBlock && leaf.Lines().Len() == 0 {
		if leaf = leaf.FirstChild(); leaf != nil {
			want += containers(leaf)
		}
	}
	if leaf == nil || leaf.Type() != ast.TypeBlock {
		return false
	}
	return markers(src[s:leaf.Lines().At(0).Start]) == want
}

// containers is 1 for a quote or list item, else 0.
func containers(n ast.Node) int {
	switch n.(type) {
	case *ast.Blockquote, *ast.ListItem:
		return 1
	}
	return 0
}

// markers counts the '>' and list markers at the start of a line, up to the first byte that is
// neither one nor a space ("> # h" has one).
func markers(line []byte) int {
	n := 0
	for i := 0; i < len(line); {
		switch line[i] {
		case ' ', '\t':
			i++
		case '>':
			n, i = n+1, i+1
		default:
			m := listMarker(line[i:])
			if m == 0 {
				return n
			}
			n, i = n+1, i+m
		}
	}
	return n
}

// listMarker is the length of a list marker ("-", "*", "+", or 1-9 digits and "." or ")") and the
// space or tab after it at the start of line, or 0.
func listMarker(line []byte) int {
	n := 0
	switch {
	case len(line) > 0 && (line[0] == '-' || line[0] == '*' || line[0] == '+'):
		n = 1
	default:
		for n < len(line) && n < 9 && line[n] >= '0' && line[n] <= '9' {
			n++
		}
		if n == 0 || n >= len(line) || (line[n] != '.' && line[n] != ')') {
			return 0
		}
		n++
	}
	if n < len(line) && line[n] != ' ' && line[n] != '\t' && line[n] != '\n' {
		return 0
	}
	return n
}

// limitNesting turns the quote or list marker past the maxNesting'th at the start of each line
// into text ('x'), in place, keeping every byte's offset. A line of rule characters is left alone:
// "- - -" is a thematic break, not three list items.
func limitNesting(src []byte) {
	for s := 0; s < len(src); {
		e := len(src)
		if i := bytes.IndexByte(src[s:], '\n'); i >= 0 {
			e = s + i
		}
		if i := deepMarker(src[s:e]); i >= 0 && !isRule(src[s:e]) {
			src[s+i] = 'x'
		}
		s = e + 1
	}
}

// deepMarker is the offset in line of the last byte of the quote or list marker that opens its
// (maxNesting+1)'th container, or -1 when it opens no more than maxNesting.
func deepMarker(line []byte) int {
	depth := 0
	for i := 0; i < len(line); {
		n := 0
		switch line[i] {
		case ' ', '\t':
			i++
			continue
		case '>':
			n = 1
		default:
			if n = listMarker(line[i:]); n == 0 {
				return -1
			}
		}
		if depth++; depth > maxNesting {
			return i + n - 1
		}
		i += n
	}
	return -1
}

// isRule says whether a line is a thematic break: three or more of one of -, * or _, and spaces.
func isRule(line []byte) bool {
	var c byte
	n := 0
	for _, b := range line {
		switch {
		case b == ' ' || b == '\t':
		case (b == '-' || b == '*' || b == '_') && (c == 0 || b == c):
			c, n = b, n+1
		default:
			return false
		}
	}
	return n >= 3
}

// toHeading is h's position in the text (src starts at offset base), its level and its text: its
// lines, trimmed and joined with a space (joinLines).
func toHeading(h *ast.Heading, src []byte, base int) heading {
	lines := h.Lines()
	first := lineStart(src, lines.At(0).Start)
	parts := make([]string, lines.Len())
	for i := range parts {
		seg := lines.At(i)
		parts[i] = string(seg.Value(src))
	}
	end := lineEnd(src, lines.At(lines.Len()-1).Start)
	setext := !bytes.ContainsRune(src[first:lines.At(0).Start], '#') // an ATX heading's text follows its '#'s
	if setext {
		end = lineEnd(src, end) // the underline
	}
	text := joinLines(strings.Join(parts, "\n"))
	return heading{start: base + first, end: base + end, level: h.Level, text: text, setext: setext}
}

// textEnd is the end of a setext heading's text lines in text: the start of its underline.
func (h heading) textEnd(text string) int {
	u := strings.LastIndexByte(strings.TrimSuffix(text[h.start:h.end], "\n"), '\n')
	return h.start + u + 1
}

// joinLines trims a heading's lines and joins those left with a space.
func joinLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, " ")
}

// lineStart is the start of the line holding offset off.
func lineStart(src []byte, off int) int { return bytes.LastIndexByte(src[:off], '\n') + 1 }

// lineEnd is the end of the line holding offset off, past its '\n'.
func lineEnd(src []byte, off int) int {
	if off >= len(src) {
		return len(src)
	}
	if i := bytes.IndexByte(src[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(src)
}
