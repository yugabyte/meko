package service

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// UploadMIMEByExt maps accepted upload extensions to their MIME type. It is the
// single source of truth for what the KB upload paths accept, mirroring the
// pipeline's SUPPORTED_DOCUMENT_TYPES (yugabyte-db/python/ai/.../rag_pipeline/
// document_types.py); unsupported types (e.g. video/mp4) are omitted so a file
// can't sit QUEUED forever. Everything below derives from this map.
var UploadMIMEByExt = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".text": "text/plain",
	".md":   "text/markdown",
	".json": "application/json",
	".csv":  "text/csv",
	".xml":  "text/xml",
	".html": "text/html",
	".htm":  "text/html",
}

// friendlyTypeName names a MIME type the way a user would, for use inside a
// sentence. Uploaders are not developers: "text/html; charset=utf-8" tells them
// nothing they can act on, while "a web page" does. Parameters are stripped
// because mimetype reports them (charset) and they never carry meaning here.
// An unrecognized type falls back to the raw value rather than a vague "some
// other type" — by then it is a file we don't accept at all, and the raw name
// is the only clue support has.
func friendlyTypeName(mime string) string {
	switch strings.TrimSpace(strings.Split(mime, ";")[0]) {
	case "application/pdf":
		return "a PDF"
	case "text/html", "application/xhtml+xml":
		return "a web page"
	case "text/xml", "application/xml":
		return "an XML file"
	case "application/json":
		return "a JSON file"
	case "text/csv":
		return "a CSV spreadsheet"
	case "text/markdown":
		return "Markdown"
	case "text/plain":
		return "plain text"
	default:
		return mime
	}
}

// ContentMismatchMessage explains a file whose contents disagree with its name.
func ContentMismatchMessage(ext, detected string) string {
	return fmt.Sprintf(
		"This %s file doesn't look like %s inside — it looks like %s. "+
			"Rename it with the matching extension, or upload the original file.",
		ext, friendlyTypeName(UploadMIMEByExt[ext]), friendlyTypeName(detected))
}

// SupportedExtensions is the sorted, comma-joined accept-list shown in
// "unsupported file" errors.
var SupportedExtensions = func() string {
	exts := make([]string, 0, len(UploadMIMEByExt))
	for ext := range UploadMIMEByExt {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return strings.Join(exts, ", ")
}()

// UnsupportedTypeMessage is shown when the file's extension is one the indexing
// pipeline can't read. A file with no extension at all reaches this too, so it
// gets its own wording rather than a sentence with a blank in it.
func UnsupportedTypeMessage(ext string) string {
	if ext == "" {
		return fmt.Sprintf(
			"This file has no extension, so we can't tell what's in it. Supported types: %s.",
			SupportedExtensions)
	}
	return fmt.Sprintf("%s files can't be added to a knowledge base. Supported types: %s.",
		ext, SupportedExtensions)
}

// ResolveUploadMIME verifies the file's bytes match its (already-accepted)
// extension and returns the MIME type to store, or ok=false to reject a renamed
// / spoofed file. The extension gives the expected type; the content must
// corroborate it. mtype comes from mimetype.Detect over the object's first
// MIMESniffLen bytes, not the whole file.
//
// When mimetype positively identifies the content (PDF, HTML, XML, JSON), it
// must equal what the extension claims — e.g. HTML saved as .txt, or a video
// saved as .pdf, is a mismatch and is rejected. The textual fallback below
// applies when the extension itself claims a type with no reliable signature
// (text/plain, text/markdown, or text/csv, i.e. .txt/.text/.md/.csv) — those
// can't be verified any other way. For a signature-bearing extension
// (.pdf/.json/.xml/.html/.htm), content that isn't positively identified as
// that type is a mismatch and is rejected, even if it happens to be textual
// (e.g. plain text renamed to report.pdf) — otherwise the fallback would
// defeat the corroboration this function exists to perform for exactly those
// extensions.
//
// text/csv is deliberately NOT in the strict/distinctive bucket even though
// mimetype can positively identify it: its detector is a fragile RFC4180
// heuristic (see https://github.com/gabriel-vasile/mimetype/issues/138), not
// a real signature — ordinary multi-line prose where every line happens to
// contain the same number of commas (e.g. "Hi Krishna, thanks.\nBye now, Krishna.")
// gets misidentified as text/csv and would otherwise be hard-rejected as
// "spoofed". Falling through to the textual check still blocks a genuinely
// binary payload (image, PDF, ...) renamed to .csv, which is all the
// corroboration a delimited-text format can reliably offer.
func ResolveUploadMIME(mtype *mimetype.MIME, ext string) (contentType, detected string, ok bool) {
	expected := UploadMIMEByExt[ext]
	detected = mtype.String()
	if expected == "" {
		return "", detected, false
	}
	if identifiedType, identified := signatureMIME(mtype); identified {
		if identifiedType != expected && !markupEquivalent(mtype, expected) {
			return "", detected, false
		}
		return expected, detected, true
	}
	// text/xml joins the textual fallback alongside text/plain/markdown/csv:
	// mimetype's XML detector requires a leading `<?xml` declaration, which
	// is optional in the spec — `<root/>` is valid XML with no signature to
	// match, so undeclared XML must be corroborated as text instead, same as
	// the client-side sniffer already does (see sniffFileType.ts).
	if expected != "text/plain" && expected != "text/markdown" &&
		expected != "text/csv" && expected != "text/xml" {
		return "", detected, false
	}
	if !mimeIsTextual(mtype) {
		return "", detected, false
	}
	return expected, detected, true
}

// signatureMIME returns the supported MIME type mimetype positively detected
// from the content, if any. text/plain, text/markdown, and text/csv are
// intentionally excluded: text/plain and text/markdown carry no signature at
// all (markdown is indistinguishable from plain text), and text/csv's
// "signature" is a fragile line-shape heuristic prone to false positives on
// ordinary prose (see ResolveUploadMIME) — none of the three can be verified
// against the extension this strictly, so they fall through to the textual
// check instead.
// Each case tests the whole ancestry, not just the detected node. mimetype
// reports the most specific type it can, and the specific ones are children of
// the general: an RSS feed comes back as application/rss+xml whose parent is
// text/xml, and Atom, XHTML and KML likewise. Comparing only the node would
// call every one of those a spoofed .xml.
func signatureMIME(m *mimetype.MIME) (string, bool) {
	switch {
	case mimeDescendsFrom(m, "application/pdf"):
		return "application/pdf", true
	case mimeDescendsFrom(m, "text/html"):
		return "text/html", true
	case mimeDescendsFrom(m, "text/xml"):
		return "text/xml", true
	case mimeDescendsFrom(m, "application/json"):
		return "application/json", true
	}
	return "", false
}

// markupEquivalent reports whether content already identified as one markup
// type legitimately answers to the extension's type as well.
//
// XHTML is the case that needs it: it is simultaneously valid HTML and valid
// XML, and mimetype files it under text/xml — so without this it would be
// accepted as .xml and rejected as .html, though both names are correct for it.
func markupEquivalent(m *mimetype.MIME, expected string) bool {
	if !m.Is("application/xhtml+xml") {
		return false
	}
	return expected == "text/html" || expected == "text/xml"
}

// mimeDescendsFrom reports whether m is t, or any descendant of t, in
// mimetype's type hierarchy.
func mimeDescendsFrom(m *mimetype.MIME, t string) bool {
	for ; m != nil; m = m.Parent() {
		if m.Is(t) {
			return true
		}
	}
	return false
}

// mimeIsTextual reports whether the content is genuinely text rather than
// binary — everything textual sits under text/plain.
func mimeIsTextual(m *mimetype.MIME) bool {
	return mimeDescendsFrom(m, "text/plain")
}

// MIMESniffLen is how many leading bytes identify a file. It matches meko_ui's
// SNIFF_BYTES (src/lib/sniffFileType.ts), so the browser's courtesy check and
// this one judge the same bytes. mimetype itself reads up to 4096; raising
// this means raising SNIFF_BYTES with it.
const MIMESniffLen = 3072

// ContentRejectedError marks a rejection caused by the file itself, as distinct from
// a failure to fetch it. The two must not collapse into one error: "we looked
// and it's a video" is the caller's problem and a 400, while "we couldn't
// look" is ours and a 500, and treating the second as the first would report a
// transient S3 blip as a spoofed file.
//
// The pending-upload watcher turns on the same distinction, where the stakes are
// higher than a status code: rejected means delete the object, while
// couldn't-look means try again next pass.
type ContentRejectedError struct{ Reason string }

func (e *ContentRejectedError) Error() string { return e.Reason }

// VerifyUploadedBytes checks that an object's leading bytes match the type its
// extension claims, returning the MIME type to store.
//
// On the presigned path the bytes go straight to the bucket, so this is the
// only point at which anything of ours reads them. meko_ui sniffs client-side
// before uploading, which is what makes a spoofed file fail immediately and
// legibly — but anything driving the presigned URL directly skips that code
// entirely (curl does), so the client check is a courtesy and this one is the
// enforcement.
func VerifyUploadedBytes(key string, head []byte) (contentType string, err error) {
	ext := strings.ToLower(path.Ext(key))
	if _, accepted := UploadMIMEByExt[ext]; !accepted {
		return "", &ContentRejectedError{UnsupportedTypeMessage(ext)}
	}
	if len(head) == 0 {
		return "", &ContentRejectedError{"This file is empty, so there's nothing to index."}
	}

	contentType, detected, ok := ResolveUploadMIME(mimetype.Detect(head), ext)
	if !ok {
		return "", &ContentRejectedError{ContentMismatchMessage(ext, detected)}
	}
	return contentType, nil
}

// VerifyObjectContent reads the head of an already-uploaded object and verifies
// it. The read is a ranged GET, so cost is bounded by MIMESniffLen rather than by
// the size of the file.
func VerifyObjectContent(
	ctx context.Context, s3Svc *S3Service, key string,
) (contentType string, err error) {
	head, err := s3Svc.ReadObjectHead(ctx, key, MIMESniffLen)
	if err != nil {
		return "", fmt.Errorf("read head of %s: %w", key, err)
	}
	return VerifyUploadedBytes(key, head)
}
