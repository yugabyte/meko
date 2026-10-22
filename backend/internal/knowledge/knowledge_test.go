package knowledge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTypeAndSniff(t *testing.T) {
	for _, name := range []string{"a.pdf", "A.TXT", "b.text", "c.md", "d.csv", "e.json", "f.xml", "g.html", "h.htm"} {
		if _, err := TypeOf(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", "a.docx", "noext", "a.pdf.exe"} {
		if _, err := TypeOf(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", name, err)
		}
	}
	ok := map[string]string{
		"a.pdf": "%PDF-1.7\n", "a.txt": "hello", "a.json": "\ufeff  [1]", "a.xml": "<?xml?><a/>",
		"a.html": "\n<!doctype html>", "a.md": "# x", "a.csv": "a,b\n1,2", "blank.json": "   ",
		// A head cut inside a UTF-8 sequence: the rest of it is past SniffBytes.
		"cut.txt": "caf\xc3",
	}
	for name, head := range ok {
		if err := Sniff(name, []byte(head)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"a.pdf": "hello", "a.txt": "bin\x00ary", "b.txt": "\xff\xfe bad", "a.json": "hello",
		"a.xml": "not xml", "a.html": "plain", "empty.txt": "",
	}
	for name, head := range bad {
		if err := Sniff(name, []byte(head)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if err := CheckSize(0); !errors.Is(err, ErrInvalid) {
		t.Error("an empty file was taken")
	}
	if err := CheckSize(MaxFileBytes + 1); !errors.Is(err, ErrInvalid) {
		t.Error("a file over 50 MiB was taken")
	}
	if err := CheckSize(MaxFileBytes); err != nil {
		t.Error(err)
	}
	if got := SanitizeFilename("../My Report (v2).pdf"); got != "My_Report__v2_.pdf" {
		t.Errorf("sanitized %q", got)
	}
}

func words(n int, word string) string {
	return strings.TrimSpace(strings.Repeat(word+" ", n))
}

func TestSplitText(t *testing.T) {
	text := words(300, "alpha") + "\n\n" + words(300, "beta") + "\n\n" + "short paragraph"
	chunks := splitText(text, ChunkSize, ChunkOverlap)
	if len(chunks) < 4 {
		t.Fatalf("want at least 4 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if runes(c) > ChunkSize {
			t.Errorf("chunk %d is %d characters", i, runes(c))
		}
		if c != strings.TrimSpace(c) || c == "" {
			t.Errorf("chunk %d isn't trimmed: %q", i, c)
		}
	}
	// Consecutive windows of one paragraph overlap: a chunk starts with the last words of the one before.
	tail := chunks[0][len(chunks[0])-50:]
	if !strings.Contains(chunks[1], strings.TrimSpace(tail)) {
		t.Errorf("chunks 0 and 1 don't overlap:\n%q\n%q", chunks[0], chunks[1][:80])
	}
	// Short paragraphs are packed together.
	small := splitText("one\n\ntwo\n\nthree", ChunkSize, ChunkOverlap)
	if len(small) != 1 || small[0] != "one\n\ntwo\n\nthree" {
		t.Errorf("small paragraphs: %q", small)
	}
	// No separator at all: cut between characters.
	long := strings.Repeat("x", 2500)
	for i, c := range splitText(long, ChunkSize, ChunkOverlap) {
		if runes(c) > ChunkSize {
			t.Errorf("unbroken chunk %d is %d characters", i, runes(c))
		}
	}
	// Multibyte text is measured in characters.
	for _, c := range splitText(strings.Repeat("é ", 1500), ChunkSize, ChunkOverlap) {
		if runes(c) > ChunkSize {
			t.Errorf("a chunk is %d characters", runes(c))
		}
	}
}

func TestChunksByType(t *testing.T) {
	ctx := context.Background()

	csv := "name,city\n" + strings.Repeat("Ada Lovelace,London\n", 200)
	got, err := Chunks(ctx, "people.csv", []byte(csv))
	if err != nil || len(got) < 3 {
		t.Fatalf("csv: %d chunks, %v", len(got), err)
	}
	for _, c := range got {
		if runes(c) > ChunkSize || strings.HasPrefix(c, "Lovelace") {
			t.Errorf("a csv chunk cuts a line or is too long: %q", c[:min(40, len(c))])
		}
	}

	md := "---\ntitle: \"Runbook\"\n---\nIntro before any heading.\n\n# Setup\nInstall it.\n\n## Linux\n" +
		"```\n# not a heading\n```\nUse apt.\n\nTroubleshooting\n---------------\n" + words(400, "retry")
	got, err = Chunks(ctx, "notes.md", []byte(md))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Document: Runbook\n\nIntro before any heading.",
		"Document: Runbook\nSection: Setup\n\nInstall it.",
		"Document: Runbook\nSection: Setup > Linux\n\n```\n# not a heading\n```\nUse apt.",
	}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("md chunk %d:\n%q\nwant\n%q", i, got[min(i, len(got)-1)], w)
		}
	}
	rest := got[len(want):]
	if len(rest) < 2 {
		t.Fatalf("the long setext section wasn't cut: %d chunks", len(rest))
	}
	for _, c := range rest {
		if !strings.HasPrefix(c, "Document: Runbook\nSection: Setup > Troubleshooting\n\nretry") || runes(c) > ChunkSize {
			t.Errorf("a cut section's chunk: %q (%d)", c[:min(60, len(c))], runes(c))
		}
	}
	got, err = Chunks(ctx, "plain.md", []byte("no headings here"))
	if err != nil || len(got) != 1 || got[0] != "Document: plain\n\nno headings here" {
		t.Errorf("a heading-less markdown file: %q %v", got, err)
	}

	page := `<!doctype html><html><head><title>Ops  Guide</title><style>p{color:red}</style>
<script>var secret = "nope";</script></head><body><nav>Home</nav><h1>Deploy</h1><p>Run the
<b>deploy</b> script.</p><ul><li>one</li><li>two</li></ul><h2>Rollback</h2><pre>keep
  spacing</pre><table><tr><td>a</td><td>b</td></tr></table></body></html>`
	got, err = Chunks(ctx, "guide.html", []byte(page))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n---\n")
	for _, w := range []string{"Document: Ops Guide\n\nHome", "Section: Deploy\n\nRun the deploy script.",
		"one\ntwo", "Section: Deploy > Rollback", "keep\n  spacing", "a b"} {
		if !strings.Contains(joined, w) {
			t.Errorf("html chunks lack %q:\n%s", w, joined)
		}
	}
	if strings.Contains(joined, "secret") || strings.Contains(joined, "color") {
		t.Errorf("scripts or styles leaked into the text:\n%s", joined)
	}

	// JSON and XML are chunked as they are; text loses its BOM and CRs.
	for name, c := range map[string]struct{ data, want string }{
		"a.json": {`{"k": "v"}`, `{"k": "v"}`},
		"a.xml":  {"<a>b</a>", "<a>b</a>"},
		"a.txt":  {"\ufeffhi\r\nthere", "hi\nthere"},
	} {
		got, err = Chunks(ctx, name, []byte(c.data))
		if err != nil || len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, c.want)
		}
	}

	if _, err = Chunks(ctx, "a.txt", []byte("  \n\n ")); !errors.Is(err, ErrNoText) {
		t.Errorf("blank text: %v", err)
	}
	if _, err = Chunks(ctx, "a.txt", []byte("bad \xff utf8")); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid UTF-8: %v", err)
	}
}

// minimalPDF is a valid PDF with one page per entry, each line of it drawn word by word, the way
// many PDF writers draw text.
func minimalPDF(pages ...string) []byte {
	objs := make([]string, 0, 2*len(pages))
	pageIDs := make([]string, 0, len(pages))
	n := 3 // 1 catalog, 2 pages, 3 font, then a page and its contents per page
	for _, text := range pages {
		var stream strings.Builder
		stream.WriteString("BT /F1 12 Tf\n")
		y := 750
		for _, line := range strings.Split(text, "\n") {
			x := 72
			for _, w := range strings.Fields(line) {
				fmt.Fprintf(&stream, "1 0 0 1 %d %d Tm (%s) Tj\n", x, y, w)
				x += 7*len(w) + 6 // Helvetica's average width at 12 pt, plus a space
			}
			y -= 16
		}
		stream.WriteString("ET")
		page, contents := n+1, n+2
		n += 2
		pageIDs = append(pageIDs, fmt.Sprintf("%d 0 R", page))
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R "+
				"/Resources << /Font << /F1 3 0 R >> >> >>", contents),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", stream.Len(), stream.String()))
	}
	all := append([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(pageIDs, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	}, objs...)
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(all))
	for i, o := range all {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(all)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(all)+1, xref)
	return b.Bytes()
}

func TestPDF(t *testing.T) {
	ctx := context.Background()
	data := minimalPDF("Quarterly report\nRevenue grew in every region", "Second page about hiring plans")
	got, err := Chunks(ctx, "report.pdf", data)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(got, "\n")
	for _, w := range []string{"Quarterly report", "Revenue grew in every region", "Second page about hiring plans"} {
		if !strings.Contains(text, w) {
			t.Errorf("the PDF's text lacks %q: %q", w, text)
		}
	}
	if _, err = Chunks(ctx, "bad.pdf", []byte("%PDF-1.4\ngarbage")); !errors.Is(err, ErrInvalid) {
		t.Errorf("a broken PDF: %v", err)
	}
	if _, err = Chunks(ctx, "blank.pdf", minimalPDF("")); !errors.Is(err, ErrNoText) {
		t.Errorf("a PDF with no text: %v", err)
	}
	// The cap on extracted text: a page's text past it is cut.
	if s, perr := pdfText(ctx, data, 10); perr != nil || len(s) != 10 {
		t.Errorf("capped text: %q %v", s, perr)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = Chunks(done, "report.pdf", data); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled extraction: %v", err)
	}
}

func TestFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "knowledgebase-files")
	f := NewFiles(root)
	if n, err := f.Sweep(nil); n != 0 || err != nil {
		t.Fatalf("sweeping a folder not made yet: %d %v", n, err)
	}
	dp := "3f2b8e0e-0000-4000-8000-000000000001"
	rec, err := f.Receive(dp, strings.NewReader("hello world"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Size != 11 || string(rec.Head) != "hello world" ||
		rec.Hash != "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" {
		t.Fatalf("received %+v", rec)
	}
	if err = f.Keep(dp, rec); err != nil {
		t.Fatal(err)
	}
	b, err := f.Read(dp, rec.Hash)
	if err != nil || string(b) != "hello world" {
		t.Fatalf("read back %q %v", b, err)
	}
	p, _ := f.Path(dp, rec.Hash)
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	for _, d := range []string{filepath.Dir(p), root} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: folder mode %v", d, fi.Mode().Perm())
		}
	}
	if _, err = f.Receive(dp, strings.NewReader("too long"), 3); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a file over its limit: %v", err)
	}
	if _, err = f.Path("../x", rec.Hash); err == nil {
		t.Error("a datapack id with a separator was taken")
	}
	if _, err = f.Path(dp, "../../etc"); err == nil {
		t.Error("a hash that isn't one was taken")
	}

	// Sweep: an unknown datapack's folder, a stray file, an old temp file and an unwanted hash go;
	// a fresh temp file and a wanted hash stay.
	other := "3f2b8e0e-0000-4000-8000-000000000002"
	orec, _ := f.Receive(other, strings.NewReader("other"), 100)
	_ = f.Keep(other, orec)
	unwanted, _ := f.Receive(dp, strings.NewReader("unwanted"), 100)
	_ = f.Keep(dp, unwanted)
	fresh, _ := f.Receive(dp, strings.NewReader("still uploading"), 100)
	stale, _ := f.Receive(dp, strings.NewReader("crashed"), 100)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale.temp, old, old)
	_ = os.WriteFile(filepath.Join(root, "stray"), []byte("x"), 0o600)
	n, err := f.Sweep(func(datapack, hash string) (bool, error) {
		return datapack == dp && (hash == "" || hash == rec.Hash), nil
	})
	if err != nil || n != 4 {
		t.Fatalf("swept %d, %v", n, err)
	}
	for path, want := range map[string]bool{p: true, fresh.temp: true, stale.temp: false,
		filepath.Join(root, other): false, filepath.Join(root, "stray"): false} {
		if _, serr := os.Stat(path); (serr == nil) != want {
			t.Errorf("%s: kept %v, want %v", path, serr == nil, want)
		}
	}
	if err = f.Remove(dp, rec.Hash); err != nil {
		t.Fatal(err)
	}
	if err = f.Remove(dp, rec.Hash); err != nil {
		t.Errorf("removing a file already gone: %v", err)
	}
	if err = f.RemoveDatapack(dp); err != nil {
		t.Fatal(err)
	}
}

func TestDeadline(t *testing.T) {
	if Deadline(1) != 2*time.Minute || Deadline(50<<20) != 7*time.Minute {
		t.Errorf("deadlines %v %v", Deadline(1), Deadline(50<<20))
	}
}
