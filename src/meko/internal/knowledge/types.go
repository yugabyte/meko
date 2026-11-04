// Package knowledge is what Meko does with a knowledge file before it reaches the database: the
// types it takes and the check that a file's bytes match its type, its text extraction and its
// chunking, and the folder its bytes live in. It runs no queries and calls no engine.
package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxFileBytes is the largest file Meko takes: 50 MiB, as Meko Cloud's standard tier.
const MaxFileBytes = 50 << 20

// SniffBytes is how much of a file's start the type check reads, as Meko Cloud's does.
const SniffBytes = 3072

// types are the extensions Meko takes and each one's MIME type, as Meko Cloud's.
var types = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".text": "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".json": "application/json",
	".xml":  "text/xml",
	".html": "text/html",
	".htm":  "text/html",
}

// Extensions is the extensions Meko takes, for messages.
const Extensions = ".pdf, .txt, .text, .md, .csv, .json, .xml, .html, .htm"

// ErrInvalid is a file Meko won't take; its message says why, for the caller.
var ErrInvalid = errors.New("invalid file")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Ext is a filename's extension, lowercased.
func Ext(filename string) string { return strings.ToLower(filepath.Ext(filename)) }

// TypeOf is the MIME type of a file Meko takes, by its name, or an ErrInvalid error.
func TypeOf(filename string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", invalid("filename is required")
	}
	t, ok := types[Ext(filename)]
	if !ok {
		return "", invalid("%q isn't a type Meko takes: use %s", filepath.Base(filename), Extensions)
	}
	return t, nil
}

// CheckSize refuses an empty file or one over MaxFileBytes.
func CheckSize(size int64) error {
	switch {
	case size <= 0:
		return invalid("this file is empty")
	case size > MaxFileBytes:
		return invalid("this file is %.1f MiB; the most Meko takes is 50 MiB", float64(size)/(1<<20))
	}
	return nil
}

// Sniff checks that a file's first bytes (up to SniffBytes) fit its extension, as Meko Cloud
// does: a PDF signature for PDFs, UTF-8 text without NUL bytes for the rest, and for JSON, XML and
// HTML a first character that fits. head may end inside a UTF-8 sequence.
func Sniff(filename string, head []byte) error {
	if _, err := TypeOf(filename); err != nil {
		return err
	}
	if len(head) > SniffBytes {
		head = head[:SniffBytes]
	}
	if len(head) == 0 {
		return invalid("this file is empty")
	}
	ext := Ext(filename)
	if ext == ".pdf" {
		if !bytes.HasPrefix(head, []byte("%PDF-")) {
			return invalid("%s doesn't look like a PDF", filepath.Base(filename))
		}
		return nil
	}
	if bytes.IndexByte(head, 0) >= 0 || !validUTF8Prefix(head) {
		return invalid("%s isn't UTF-8 text", filepath.Base(filename))
	}
	text := strings.TrimLeft(strings.TrimPrefix(string(head), bom), " \t\r\n")
	if text == "" {
		return nil // only whitespace so far: nothing to contradict the type
	}
	switch ext {
	case ".json":
		if text[0] != '{' && text[0] != '[' {
			return invalid("%s doesn't look like JSON", filepath.Base(filename))
		}
	case ".xml", ".html", ".htm":
		if text[0] != '<' {
			return invalid("%s doesn't look like %s", filepath.Base(filename), strings.ToUpper(ext[1:]))
		}
	}
	return nil
}

const bom = "\xef\xbb\xbf"

// validUTF8Prefix reports whether b is UTF-8, allowing it to end inside a sequence.
func validUTF8Prefix(b []byte) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n <= 1 {
			return !utf8.FullRune(b) // a cut-off sequence at the end is fine
		}
		b = b[n:]
	}
	return true
}

// SanitizeFilename is a filename as Meko Cloud keys it: anything but letters, digits, '_', '-' and
// '.' becomes '_'.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// IsHash reports whether s is a SHA-256 in lowercase hex.
func IsHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
