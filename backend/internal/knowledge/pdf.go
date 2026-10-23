package knowledge

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// pdfText is a PDF's text, page by page, at most limit bytes. Pieces of text are taken in the order
// the page draws them, with a line break where the next one starts on another line, and spaces
// between words as writeLine finds them, since PDFs often draw words, or letters, one at a time.
func pdfText(ctx context.Context, data []byte, limit int) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if errors.Is(err, pdf.ErrInvalidPassword) {
		return "", invalid("the PDF is encrypted")
	}
	if err != nil {
		return "", invalid("the PDF couldn't be read")
	}
	var out strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		writePage(&out, p.Content().Text)
		if out.Len() > limit {
			return cutText(out.String(), limit), nil
		}
	}
	return out.String(), nil
}

// writePage writes one page's pieces of text, a line at a time.
func writePage(out *strings.Builder, texts []pdf.Text) {
	var line []pdf.Text
	for _, t := range texts {
		if t.S == "" {
			continue
		}
		if n := len(line); n > 0 {
			prev := line[n-1]
			if size := math.Max(math.Max(prev.FontSize, t.FontSize), 1); math.Abs(t.Y-prev.Y) > size/2 {
				writeLine(out, line)
				out.WriteString("\n")
				line = line[:0]
			}
		}
		line = append(line, t)
	}
	writeLine(out, line)
}

// spacedOut is how much longer than a line's usual step from one piece to the next a step must be
// to be read as a word break, when the PDF gives neither widths nor spaces.
const spacedOut = 1.5

// writeLine writes one line's pieces, with a space wherever a word break falls. Where to put one
// depends on what the PDF says:
//   - Widths: a gap between one piece's end and the next one's start over 0.15 of the font size.
//   - No widths (design tools often leave them 0, so every letter's step reads as a gap) but its own
//     space characters: those, and no more. Adding gaps then split every word into its letters.
//   - Neither, letters stacked (a word's letters all at its start, as some PDFs draw a word at a
//     time): any step forward, which is the next word.
//   - Neither, letters spread out: a step from one piece's start to the next much longer than the
//     line's usual one.
func writeLine(out *strings.Builder, line []pdf.Text) {
	widths, drawnSpaces := false, false
	for _, t := range line {
		widths = widths || t.W > 0
		drawnSpaces = drawnSpaces || strings.Contains(t.S, " ")
	}
	var usual float64 // the median step, for a line with neither
	stacked := false  // some pieces start where the one before did
	if !widths && !drawnSpaces {
		var steps []float64
		for i := 1; i < len(line); i++ {
			switch d := line[i].X - line[i-1].X; {
			case math.Abs(d) < 0.01:
				stacked = true
			case d > 0:
				steps = append(steps, d)
			}
		}
		if len(steps) > 0 {
			sort.Float64s(steps)
			usual = steps[len(steps)/2]
		}
	}
	for i, t := range line {
		if i > 0 {
			prev := line[i-1]
			joined := strings.HasSuffix(prev.S, " ") || strings.HasPrefix(t.S, " ")
			var space bool
			switch {
			case widths:
				space = t.X-(prev.X+prev.W) > math.Max(math.Max(prev.FontSize, t.FontSize), 1)*0.15
			case drawnSpaces:
			case stacked:
				space = t.X-prev.X >= 0.01
			case usual > 0:
				space = t.X-prev.X > usual*spacedOut
			}
			if space && !joined {
				out.WriteString(" ")
			}
		}
		out.WriteString(t.S)
	}
}
