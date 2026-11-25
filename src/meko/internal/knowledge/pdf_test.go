package knowledge

import (
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// glyphs lays s out one piece per character on a line at y, each 6 wide, with width w (0 for a PDF
// that gives none); a | after a letter widens the next step by gap, as a word break does.
func glyphs(s string, y, w, gap float64) []pdf.Text {
	const step = 6.0
	out := make([]pdf.Text, 0, len(s))
	x := 10.0
	for _, r := range s {
		out = append(out, pdf.Text{S: string(r), X: x, Y: y, W: w, FontSize: 10})
		x += step
		if r == '|' {
			x += gap
		}
	}
	return out
}

func page(pieces ...[]pdf.Text) string {
	var all []pdf.Text
	for _, p := range pieces {
		all = append(all, p...)
	}
	var b strings.Builder
	writePage(&b, all)
	return strings.ReplaceAll(b.String(), "|", "")
}

func TestPDFSpacing(t *testing.T) {
	cases := map[string]struct {
		pieces []pdf.Text
		want   string
	}{
		// Widths: a piece's end to the next one's start, over 0.15 of the font size, is a space.
		"widths, letters touching": {glyphs("Shivam|Dubey", 100, 6, 4), "Shivam Dubey"},
		// Design tools leave widths 0 but draw their spaces: use those, add none.
		"no widths, drawn spaces": {glyphs("Shivam Dubey", 100, 0, 0), "Shivam Dubey"},
		// Neither: a word break can't be told from a wide letter's step, so only a column gap is one.
		"no widths, no spaces, small gaps":   {glyphs("Shivam|Dubey", 100, 0, 6), "ShivamDubey"},
		"no widths, no spaces, column gap":   {glyphs("Shivam|Dubey", 100, 0, 30), "Shivam Dubey"},
		"no widths, no spaces, wide letters": {proportional("Community"), "Community"},
		// One piece with a width (a space glyph) doesn't make a width-less line use the gap rule.
		"no widths but one space with a width": {oneWideSpace(), "Shivam Dubey"},
		// One letter drawn twice at its spot doesn't make a spread-out line stacked.
		"spread out with one doubled letter": {doubled(), "SShivam Dubey"},
		// Stacked words, one holding a drawn space: the steps forward still break words.
		"stacked words with a drawn space": {stacked("Dr. Smith", "joined"), "Dr. Smith joined"},
		// Drawn spaces, and a layout gap with none in it: a company on the left, its city on the right.
		"no widths, drawn spaces and a column gap": {columns(), "Shivam Dubey Quantiphi Bengaluru"},
		// Neither, a word's letters stacked at its start: each step forward is the next word.
		"no widths, letters stacked": {stacked("Shivam", "Dubey"), "Shivam Dubey"},
	}
	for name, tc := range cases {
		if got := page(tc.pieces); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	// Lines break where the next piece is more than half the font size above or below, each line
	// decided alone.
	two := page(glyphs("Shivam Dubey", 100, 0, 0), glyphs("Go|Rust", 80, 6, 4))
	if two != "Shivam Dubey\nGo Rust" {
		t.Errorf("two lines: %q", two)
	}
}

// stacked draws each word's letters all at the word's start, as minimalPDF's PDF comes out.
func stacked(words ...string) []pdf.Text {
	var out []pdf.Text
	x := 10.0
	for _, w := range words {
		for _, r := range w {
			out = append(out, pdf.Text{S: string(r), X: x, Y: 100, FontSize: 12})
		}
		x += 40
	}
	return out
}

// columns is "Shivam Dubey" with its space drawn, then "Quantiphi" and, far to its right with no
// space drawn between, "Bengaluru", all on one line with no widths.
func columns() []pdf.Text {
	out := glyphs("Shivam Dubey ", 100, 0, 0)
	q := glyphs("Quantiphi", 100, 0, 0)
	for i := range q {
		q[i].X += 80
	}
	b := glyphs("Bengaluru", 100, 0, 0)
	for i := range b {
		b[i].X += 400
	}
	return append(append(out, q...), b...)
}

// proportional draws a word one letter at a time with no widths, each letter's step its own advance
// in a proportional font, so a wide "m" steps far further than the narrow letters around it.
func proportional(word string) []pdf.Text {
	advance := map[rune]float64{'m': 8.3, 'C': 7.2, 'o': 5.6, 'u': 5.6, 'n': 5.6, 'i': 2.2, 't': 2.8, 'y': 5}
	out := make([]pdf.Text, 0, len(word))
	x := 10.0
	for _, r := range word {
		out = append(out, pdf.Text{S: string(r), X: x, Y: 100, FontSize: 10})
		a, ok := advance[r]
		if !ok {
			a = 5
		}
		x += a
	}
	return out
}

// oneWideSpace is "Shivam Dubey" with no widths, but a space glyph that has one.
func oneWideSpace() []pdf.Text {
	out := glyphs("Shivam Dubey", 100, 0, 0)
	for i := range out {
		if out[i].S == " " {
			out[i].W = 2.5
		}
	}
	return out
}

// doubled is "Shivam Dubey" spread out with no widths, its first letter drawn twice at one spot (as
// faux bold does), so it comes out twice; the point is that the rest isn't split into letters.
func doubled() []pdf.Text {
	out := glyphs("Shivam Dubey", 100, 0, 0)
	return append([]pdf.Text{out[0]}, out...)
}
