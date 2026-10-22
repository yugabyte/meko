package knowledge

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"

	"github.com/ledongthuc/pdf"
)

// pdfText is a PDF's text, page by page, at most limit bytes. Pieces of text are taken in the order
// the page draws them, with a space where there's a gap between two and a line break where the
// next one starts on another line, since PDFs often draw words, or letters, one at a time.
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

// writePage writes one page's pieces of text.
func writePage(out *strings.Builder, texts []pdf.Text) {
	var prev *pdf.Text
	for i := range texts {
		t := &texts[i]
		if t.S == "" {
			continue
		}
		if prev != nil {
			size := math.Max(math.Max(prev.FontSize, t.FontSize), 1)
			gap := t.X - (prev.X + prev.W)
			switch {
			case math.Abs(t.Y-prev.Y) > size/2:
				out.WriteString("\n")
			case gap > size*0.15 && !strings.HasSuffix(prev.S, " ") && !strings.HasPrefix(t.S, " "):
				out.WriteString(" ")
			}
		}
		out.WriteString(t.S)
		prev = t
	}
}
