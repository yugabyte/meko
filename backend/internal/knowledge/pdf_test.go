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
		// Neither: a step well past the line's usual one is a word break.
		"no widths, no spaces": {glyphs("Shivam|Dubey|builds", 100, 0, 6), "Shivam Dubey builds"},
		// Neither, a word's letters stacked at its start: each step forward is the next word.
		"no widths, letters stacked": {stacked("Shivam", "Dubey"), "Shivam Dubey"},
	}
	for name, tc := range cases {
		if got := page(tc.pieces); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	// Lines break where the next piece is lower than half the font size, each line decided alone.
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
