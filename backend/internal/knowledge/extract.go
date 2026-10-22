package knowledge

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrNoText is a file with no text to index: empty after extraction, or a PDF of images only.
var ErrNoText = errors.New("the file has no text to index")

// maxTextFactor caps the text extracted from a file at this many times its size, so a small file
// that expands enormously (a hostile PDF) can't fill the database.
const maxTextFactor = 4

// Deadline is how long extracting and chunking a file may take: 2 minutes, plus 1 minute per
// 10 MB.
func Deadline(size int64) time.Duration {
	return 2*time.Minute + time.Duration(size/(10<<20))*time.Minute
}

// Chunks extracts a file's text and cuts it into chunks for embedding, by its type (its name's
// extension). It returns ErrInvalid (wrapped, saying why) for bytes it can't read as their type,
// ErrNoText when there's no text, and ctx's error when ctx ends first. A parser that panics on a
// malformed file is an ErrInvalid too.
func Chunks(ctx context.Context, filename string, data []byte) (chunks []string, err error) {
	if _, err = TypeOf(filename); err != nil {
		return nil, err
	}
	type result struct {
		chunks []string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if p := recover(); p != nil {
				r = result{err: invalid("%s couldn't be read: its contents are malformed", filepath.Base(filename))}
			}
			done <- r
		}()
		r.chunks, r.err = extract(ctx, filename, data)
	}()
	select {
	case r := <-done:
		chunks, err = r.chunks, r.err
	case <-ctx.Done():
		// The parser has no way to be stopped; it finishes on its own and its result is dropped.
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, ErrNoText
	}
	return chunks, nil
}

func extract(ctx context.Context, filename string, data []byte) ([]string, error) {
	ext := Ext(filename)
	if ext == ".pdf" {
		text, err := pdfText(ctx, data, maxTextFactor*len(data))
		if err != nil {
			return nil, err
		}
		return splitText(text, ChunkSize, ChunkOverlap), nil
	}
	if !utf8.Valid(data) {
		return nil, invalid("%s isn't UTF-8 text", filepath.Base(filename))
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), bom), "\r\n", "\n")
	switch ext {
	case ".csv":
		return packLines(text), nil
	case ".md":
		title, sections := markdownSections(text)
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
		}
		return sectionChunks(title, sections), nil
	case ".html", ".htm":
		title, sections, err := htmlSections(text)
		if err != nil {
			return nil, err
		}
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
		}
		return sectionChunks(title, sections), nil
	}
	// Plain text, JSON and XML are chunked as they are, as Meko Cloud chunks them.
	return splitText(text, ChunkSize, ChunkOverlap), nil
}

var (
	atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	atxEmpty   = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]*$`)
	setextRule = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	fence      = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	titleLine  = regexp.MustCompile(`^title:\s*(.+?)\s*$`)
)

// markdownSections splits Markdown into sections at its headings (ATX "# x" and setext underlines),
// outside code fences. The title is front matter's title, else "".
func markdownSections(text string) (title string, sections []section) {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				for _, l := range lines[1:i] {
					if m := titleLine.FindStringSubmatch(l); m != nil {
						title = strings.Trim(m[1], `"'`)
					}
				}
				lines = lines[i+1:]
				break
			}
		}
	}
	var h headings
	cur := section{}
	var body []string
	flush := func(next []string) {
		cur.text = strings.Join(body, "\n")
		sections = append(sections, cur)
		cur, body = section{path: next}, nil
	}
	inFence := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := fence.FindStringSubmatch(line); m != nil {
			switch {
			case inFence == "":
				inFence = m[1][:1]
			case strings.HasPrefix(m[1], inFence):
				inFence = ""
			}
			body = append(body, line)
			continue
		}
		if inFence != "" {
			body = append(body, line)
			continue
		}
		if m := atxHeading.FindStringSubmatch(line); m != nil {
			flush(h.enter(len(m[1]), strings.TrimSpace(m[2])))
			continue
		}
		if atxEmpty.MatchString(line) {
			continue
		}
		// A setext heading: a line of text underlined with = (level 1) or - (level 2).
		if i+1 < len(lines) && strings.TrimSpace(line) != "" && setextRule.MatchString(lines[i+1]) &&
			!strings.HasPrefix(strings.TrimSpace(line), "-") && !strings.HasPrefix(strings.TrimSpace(line), ">") &&
			(len(body) == 0 || strings.TrimSpace(body[len(body)-1]) == "") {
			level := 2
			if strings.TrimSpace(lines[i+1])[0] == '=' {
				level = 1
			}
			flush(h.enter(level, strings.TrimSpace(line)))
			i++
			continue
		}
		body = append(body, line)
	}
	flush(nil)
	return title, sections
}

// cutText is text cut at limit bytes, on a rune boundary.
func cutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
