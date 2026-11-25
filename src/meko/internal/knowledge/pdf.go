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

// columnGap is how much longer than a line's usual step from one piece to the next a step must be
// to be read as a layout gap (a company on the left, its city on the right) when the PDF gives no
// widths: far past any letter's advance and a space.
const columnGap = 3.0

// writeLine writes one line's pieces, with a space wherever a word break falls. What the line is
// made of decides where, each by most of its pieces, so one odd piece (a space glyph that has a
// width, a letter drawn twice at one spot) can't flip the whole line:
//   - Most letters have widths: a gap between one piece's end and the next one's start over 0.15
//     of the font size.
//   - No widths, most steps zero (a word's letters all drawn at its start, a word at a time): any
//     step forward is the next word, as well as the spaces the PDF draws.
//   - No widths, letters spread out (design tools often leave widths 0, so every letter's step
//     reads as a gap): the PDF's own space characters, plus any jump past columnGap times the line's
//     usual step. Without widths a word break can't be told from a wide letter's advance (the "m"
//     in "Community"), so a line with neither spaces nor stacking gets only its column gaps: a few
//     words run together rather than every word split into letters.
func writeLine(out *strings.Builder, line []pdf.Text) {
	var letters, wide, steps, zero int
	var forward []float64
	for i, t := range line {
		if strings.TrimSpace(t.S) == "" {
			continue
		}
		letters++
		if t.W > 0 {
			wide++
		}
		if i > 0 {
			switch d := t.X - line[i-1].X; {
			case math.Abs(d) < 0.01:
				zero++
				steps++
			case d > 0:
				forward = append(forward, d)
				steps++
			}
		}
	}
	widths := wide*2 > letters
	stacked := !widths && zero*2 > steps
	var usual float64 // the median step forward
	if len(forward) > 0 {
		sort.Float64s(forward)
		usual = forward[len(forward)/2]
	}
	for i, t := range line {
		if i > 0 {
			prev := line[i-1]
			joined := strings.HasSuffix(prev.S, " ") || strings.HasPrefix(t.S, " ")
			var space bool
			switch step := t.X - prev.X; {
			case widths:
				space = t.X-(prev.X+prev.W) > math.Max(math.Max(prev.FontSize, t.FontSize), 1)*0.15
			case stacked:
				space = step >= 0.01
			default:
				space = usual > 0 && step > usual*columnGap
			}
			if space && !joined {
				out.WriteString(" ")
			}
		}
		out.WriteString(t.S)
	}
}
