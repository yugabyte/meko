package cli

import (
	"io"
	"math"
	"os"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
	"golang.org/x/text/width"

	"github.com/yugabyte/meko/backend/internal/spin"
)

// nameDesc is one row of a name and description list, such as meko-cli datapacks'.
type nameDesc struct{ name, description string }

// The gap between the name and description columns, and the least room a description gets beside
// the names before the name column stops growing.
const (
	columnGap       = 3
	minDescriptions = 30
)

// nameDescTable lays rows out as two columns: the names, then the descriptions, each description on
// one line with its whitespace collapsed. Widths are terminal cells (cells). With a width (a
// terminal's), descriptions word-wrap in their column, and the name column stops growing where
// descriptions would get less than minDescriptions, or less than their widest word (a URL, say): a
// name wider than that gets a line of its own, its description starting on the next. Names and words
// are never broken. So a word is wider than the column only when it's wider than the terminal less
// the gap: it moves left to end at the edge, or, wider than the terminal itself, starts at the left
// and runs past it. Width 0 wraps nothing.
func nameDescTable(rows []nameDesc, width int) string {
	widest := minDescriptions
	for _, r := range rows {
		for _, w := range strings.Fields(r.description) {
			widest = max(widest, cells(w))
		}
	}
	nameCap := width - columnGap - widest
	if width <= 0 {
		nameCap = math.MaxInt
	}
	col := 0
	for _, r := range rows {
		if n := cells(r.name); n <= nameCap {
			col = max(col, n)
		}
	}
	descAt := col + columnGap
	descWidth := max(width-descAt, minDescriptions/3)
	var b strings.Builder
	for _, r := range rows {
		lines := []string{strings.Join(strings.Fields(r.description), " ")}
		if width > 0 {
			lines = wrapWords(lines[0], descWidth)
		}
		if len(lines) == 0 || lines[0] == "" {
			b.WriteString(r.name + "\n")
			continue
		}
		n := cells(r.name)
		if n > col || (width > 0 && cells(lines[0]) > descWidth) {
			b.WriteString(r.name + "\n")
		} else {
			b.WriteString(r.name + strings.Repeat(" ", descAt-n) + lines[0] + "\n")
			lines = lines[1:]
		}
		for _, line := range lines {
			indent := descAt
			if l := cells(line); l > descWidth {
				indent = max(min(descAt, width-l), 0)
			}
			b.WriteString(strings.Repeat(" ", indent) + line + "\n")
		}
	}
	return b.String()
}

// wrapWords splits s into lines of at most width cells at its spaces. A word wider than width gets
// a line of its own, whole.
func wrapWords(s string, width int) []string {
	var lines []string
	line, n := "", 0
	for _, w := range strings.Fields(s) {
		wn := cells(w)
		if n > 0 && n+1+wn <= width {
			line, n = line+" "+w, n+1+wn
			continue
		}
		if n > 0 {
			lines = append(lines, line)
		}
		line, n = w, wn
	}
	if n > 0 {
		lines = append(lines, line)
	}
	return lines
}

// cells is how many terminal cells s takes: two for a wide or fullwidth rune (CJK, most emoji),
// none for a combining mark or a zero-width joiner, else one.
func cells(s string) int {
	n := 0
	for _, r := range s {
		switch k := width.LookupRune(r).Kind(); {
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		case k == width.EastAsianWide || k == width.EastAsianFullwidth:
			n += 2
		default:
			n++
		}
	}
	return n
}

// termWidth is the width of the terminal w writes to: 0 when w isn't a terminal (a character
// device that isn't one, such as /dev/null, fails the size query), 80 when the terminal doesn't
// say.
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok || !spin.IsTerminal(f) {
		return 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	if ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}
