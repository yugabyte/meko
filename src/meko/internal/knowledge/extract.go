package knowledge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrNoText is a file with no text to index: empty after extraction, or a PDF of images only.
var ErrNoText = errors.New("the file has no text to index")

// The text a file's chunks may hold in all: maxTextFactor times its size, or minTextCap for a small
// file (whose section prefixes can outweigh its text). Past it the rest is dropped, so a file that
// expands enormously (a hostile PDF, or Markdown with a huge heading over many tiny sections) can't
// fill the indexer's memory or the database.
const (
	maxTextFactor = 4
	minTextCap    = 1 << 20
)

// textCap is how much chunk text a file of size bytes may give.
func textCap(size int) int { return max(maxTextFactor*size, minTextCap) }

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
		// PDF extraction and Markdown's heading finder check ctx between pages and windows; what
		// runs meanwhile can't be stopped, and its result is dropped.
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	chunks = capChunks(chunks, textCap(len(data)))
	if len(chunks) == 0 {
		return nil, ErrNoText
	}
	return chunks, nil
}

// capChunks keeps the chunks while their text adds up to at most limit bytes.
func capChunks(chunks []string, limit int) []string {
	total := 0
	for i, c := range chunks {
		if total += len(c); total > limit {
			return chunks[:i]
		}
	}
	return chunks
}

func extract(ctx context.Context, filename string, data []byte) ([]string, error) {
	ext := Ext(filename)
	if ext == ".pdf" {
		text, err := pdfText(ctx, data, textCap(len(data)))
		if err != nil {
			return nil, err
		}
		return splitText(text, ChunkSize, ChunkOverlap), nil
	}
	if !utf8.Valid(data) {
		return nil, invalid("%s isn't UTF-8 text", filepath.Base(filename))
	}
	// Line ends become "\n": "\r\n", and a lone "\r" (classic Mac OS) too, or the file reads as one line.
	text := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(string(data), bom), "\r\n", "\n"), "\r", "\n")
	switch ext {
	case ".csv":
		return packLines(text), nil
	case ".md":
		title, sections, err := markdownSections(ctx, text)
		if err != nil {
			return nil, err
		}
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
