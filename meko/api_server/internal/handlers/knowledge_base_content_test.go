package handlers

import (
	"errors"
	"strings"
	"testing"
)

// pngHead is a PNG signature plus a complete IHDR chunk — enough for mimetype
// to identify it, and the stand-in for "binary content that isn't text".
var pngHead = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func TestVerifyUploadedBytes(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		head     []byte
		wantType string // "" means the file must be rejected
	}{
		// Content agrees with the extension.
		{"pdf", "u/d/h/report.pdf", []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n"), "application/pdf"},
		{"html", "u/d/h/page.html", []byte("<!DOCTYPE html><html><body>hi</body></html>"), "text/html"},
		{"json", "u/d/h/data.json", []byte(`{"a":1,"b":[2,3],"c":"four"}`), "application/json"},
		{"xml", "u/d/h/feed.xml", []byte(`<?xml version="1.0"?><rss><channel/></rss>`), "text/xml"},
		// The XML declaration is optional in the spec, so mimetype's
		// signature match doesn't fire here; this must fall through to the
		// same textual-corroboration path as text/plain, not be rejected.
		{"xml without declaration", "u/d/h/feed.xml", []byte(`<root/>`), "text/xml"},
		{"plain text", "u/d/h/notes.txt", []byte("just some notes\nsecond line\n"), "text/plain"},
		{"markdown is text", "u/d/h/readme.md", []byte("# Title\n\nSome prose.\n"), "text/markdown"},

		// Content contradicts the extension. Each of these is a file that the
		// presigned path would otherwise store and queue for indexing.
		{"png renamed .txt", "u/d/h/notes.txt", pngHead, ""},
		{"png renamed .pdf", "u/d/h/report.pdf", pngHead, ""},
		{"html renamed .txt", "u/d/h/notes.txt", []byte("<html><body>hi</body></html>"), ""},
		{"text renamed .pdf", "u/d/h/report.pdf", []byte("this is not a pdf at all"), ""},
		{"text renamed .html", "u/d/h/page.html", []byte("no markup anywhere here"), ""},

		// Rejected for a reason that isn't a mismatch. Names outside the
		// accept-list are covered on their own, below.
		{"empty", "u/d/h/notes.txt", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verifyUploadedBytes(tc.key, tc.head)

			if tc.wantType == "" {
				if err == nil {
					t.Fatalf("expected rejection, got content type %q", got)
				}
				// Callers branch on this type to choose 400 over 500, so a
				// rejection that isn't tagged would be reported as our fault.
				var rejected *contentRejected
				if !errors.As(err, &rejected) {
					t.Fatalf("rejection should be *contentRejected, got %T: %v", err, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("expected %s to be accepted, got: %v", tc.name, err)
			}
			if got != tc.wantType {
				t.Fatalf("content type = %q, want %q", got, tc.wantType)
			}
		})
	}
}

// Prose where every line happens to hold the same number of commas trips
// mimetype's CSV line-shape heuristic. resolveUploadMIME deliberately lets
// text/csv fall through to the textual check for that reason, and a .txt file
// must not be rejected just because it looks tabular.
func TestVerifyUploadedBytes_CSVHeuristicDoesNotRejectProse(t *testing.T) {
	prose := []byte("Hi Krishna, thanks for the note.\nBye now, Krishna.\n")

	for _, key := range []string{"u/d/h/notes.txt", "u/d/h/table.csv"} {
		if _, err := verifyUploadedBytes(key, prose); err != nil {
			t.Errorf("%s should be accepted, got: %v", key, err)
		}
	}
}

// The bytes decide, not the name: identical content is accepted or rejected
// purely on whether the extension it is wearing matches.
func TestVerifyUploadedBytes_SameBytesDifferentExtension(t *testing.T) {
	html := []byte("<!DOCTYPE html><html><body>report</body></html>")

	if _, err := verifyUploadedBytes("u/d/h/page.html", html); err != nil {
		t.Fatalf("html as .html should be accepted, got: %v", err)
	}
	_, err := verifyUploadedBytes("u/d/h/page.txt", html)
	if err == nil {
		t.Fatal("html as .txt should be rejected")
	}
	// The message names both sides, because "rejected" alone leaves the user
	// with no idea what to rename or re-export -- and it names them in words,
	// since the person reading it uploaded a file and has no reason to know
	// what a MIME type is.
	msg := err.Error()
	if !strings.Contains(msg, "plain text") || !strings.Contains(msg, "a web page") {
		t.Errorf("message should name expected and detected types, got: %v", err)
	}
	if strings.Contains(msg, "text/html") || strings.Contains(msg, "charset") {
		t.Errorf("message should not leak raw MIME types, got: %v", err)
	}
}

// The extension gate is a closed list: only the keys of uploadMimeByExt are
// accepted, and everything else is refused on its name alone, before a byte is
// read. The refusal has to name the extension it turned away and the types that
// would have worked, or the user has nothing to act on.
func TestVerifyUploadedBytes_RejectsExtensionsOutsideTheList(t *testing.T) {
	// Content that would be accepted as .txt, so the name is the only thing
	// that can be rejecting these.
	body := []byte("plain text, nothing unusual in here\n")

	for _, ext := range []string{".mp4", ".zip", ".exe", ".docx", ".yaml", ".log", ".gz", ".rtf"} {
		if _, supported := uploadMimeByExt[ext]; supported {
			t.Fatalf("%s is on the supported list; this case needs one that isn't", ext)
		}
		t.Run(ext, func(t *testing.T) {
			_, err := verifyUploadedBytes("u/d/h/file"+ext, body)
			if err == nil {
				t.Fatalf("%s should be rejected", ext)
			}
			if msg := err.Error(); !strings.Contains(msg, ext) || !strings.Contains(msg, allowedExtensionsMsg) {
				t.Errorf("message should name the refused type and the supported list, got: %q", msg)
			}
		})
	}

	// A name with nothing to go on can't be told which extension was wrong.
	_, err := verifyUploadedBytes("u/d/h/README", body)
	if err == nil {
		t.Fatal("a file with no extension should be rejected")
	}
	if !strings.Contains(err.Error(), "no extension") {
		t.Errorf("a file with no extension needs its own wording, got: %q", err.Error())
	}
}

// mimetype answers with the most specific type it can, and the specific XML
// formats are children of text/xml — so an ordinary RSS feed is reported as
// application/rss+xml, not text/xml. Matching on the detected node alone would
// call every one of these a spoofed .xml.
func TestVerifyUploadedBytes_XMLSubtypesAreValidXML(t *testing.T) {
	cases := map[string]string{
		"rss":   `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title></channel></rss>`,
		"atom":  `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>t</title></feed>`,
		"kml":   `<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document/></kml>`,
		"xhtml": `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body>x</body></html>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifyUploadedBytes("u/d/h/feed.xml", []byte(body)); err != nil {
				t.Fatalf("%s should be accepted as .xml, got: %v", name, err)
			}
		})
	}
}

// XHTML is both valid HTML and valid XML, so neither name for it is a spoof.
func TestVerifyUploadedBytes_XHTMLSatisfiesBothNames(t *testing.T) {
	xhtml := []byte(`<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body>x</body></html>`)

	for _, key := range []string{"u/d/h/page.html", "u/d/h/page.htm", "u/d/h/page.xml"} {
		if _, err := verifyUploadedBytes(key, xhtml); err != nil {
			t.Errorf("xhtml as %s should be accepted, got: %v", key, err)
		}
	}
}

// The flip side of matching on ancestry: markup is positively identified, so
// it can no longer pass itself off as .txt by falling through to the textual
// check. Consistent with HTML-as-.txt, which was always rejected.
func TestVerifyUploadedBytes_XMLCannotPassAsPlainText(t *testing.T) {
	rss := []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title></channel></rss>`)

	if _, err := verifyUploadedBytes("u/d/h/notes.txt", rss); err == nil {
		t.Fatal("rss as .txt should be rejected")
	}
}

func TestVerifyUploadedBytes_ExtensionCaseInsensitive(t *testing.T) {
	if _, err := verifyUploadedBytes("u/d/h/REPORT.PDF", []byte("%PDF-1.4\ntrailer\n")); err != nil {
		t.Fatalf("uppercase extension should be accepted, got: %v", err)
	}
}
