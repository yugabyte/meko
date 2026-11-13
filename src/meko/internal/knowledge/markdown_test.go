package knowledge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	gmtext "github.com/yuin/goldmark/text"
)

// headingCases are Markdown snippets and the top-level headings CommonMark finds in them, as
// "level:text".
var headingCases = []struct {
	name, md string
	want     []string
}{
	{"atx", "# A\ntext\n## B\n", []string{"1:A", "2:B"}},
	{"html comment", "<!--\n## old notes\n-->\n# Real\n", []string{"1:Real"}},
	{"processing instruction", "<?php\n## x\n?>\n# Real\n", []string{"1:Real"}},
	{"cdata", "<![CDATA[\n## x\n]]>\n# Real\n", []string{"1:Real"}},
	{"declaration", "<!DOCTYPE html\n## x\n>\n# Real\n", []string{"1:Real"}},
	{"pre with blank lines", "<pre>\n\n## x\n\n</pre>\n# Real\n", []string{"1:Real"}},
	{"script", "<script>\n## x\n</script>\n# Real\n", []string{"1:Real"}},
	{"div block", "<div>\n## x\n</div>\n\n# Real\n", []string{"1:Real"}},
	{"div then blank then heading", "<div>\n\n## H\n", []string{"2:H"}},
	{"custom tag alone", "<custom-box>\n## x\n</custom-box>\n\n# Real\n", []string{"1:Real"}},
	{"custom tag after text", "text\n<custom-box>\n## H\n", []string{"2:H"}},
	{"inline html", "Some <b>bold</b> text\n## H\n", []string{"2:H"}},
	{"indented comment is code", "    <!--\n## H\n", []string{"2:H"}},
	{"short fence inside a long one", "````\n```\n# x\n````\n# Real\n", []string{"1:Real"}},
	{"closer with text after it", "```\n```bash\n# x\n```\n# Real\n", []string{"1:Real"}},
	{"backtick in a backtick label", "``` a`b\n# H\n", []string{"1:H"}},
	{"tab-indented closer", "```\n\t```\n# x\n```\n# Real\n", []string{"1:Real"}},
	{"4-space closer", "```\n    ```\n# x\n```\n# Real\n", []string{"1:Real"}},
	{"tilde fence", "~~~\n# x\n~~~\n# Real\n", []string{"1:Real"}},
	{"fence in a quote", "> ```\n> # x\n> ```\n# Real\n", []string{"1:Real"}},
	{"fence in a list item", "- ```\n  # x\n  ```\n# Real\n", []string{"1:Real"}},
	{"hashtag", "#hashtag\n", nil},
	{"7 hashes", "####### x\n", nil},
	{"closing hashes", "## Install ##\n", []string{"2:Install"}},
	{"3-space indent", "   # A\n", []string{"1:A"}},
	{"4-space indent is code", "    # A\n", nil},
	{"heading after a paragraph", "para\n## H\n", []string{"2:H"}},
	{"setext", "Title\n-----\n", []string{"2:Title"}},
	{"multi-line setext", "line one\nline two\n===\n", []string{"1:line one line two"}},
	{"quote then heading", "> quote\n## H\n", []string{"2:H"}},
	{"link definition then ===", "[a]: /url\n===\n", []string{"1:[a]: /url"}},
	{"link definition then ---", "[a]: /url\n---\n", []string{"2:[a]: /url"}},
	{"link definition in a setext heading", "[a]: /url\nfoo\n===\n", []string{"1:[a]: /url foo"}},
	{"rule in a quote, then a setext heading", "> " + strings.TrimSpace(strings.Repeat("- ", 40)) + "\nTitle\n===\n",
		[]string{"1:Title"}},
	{"empty heading", "#\ntext\n# A\n", []string{"1:A"}},
	{"list over a rule", "- item\n---\n", nil},
	{"heading in a list item", "- # Title\n", nil},
	{"heading in a quote", "> # Title\n", nil},
}

func headingStrings(hs []heading) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, fmt.Sprintf("%d:%s", h.level, h.text))
	}
	return out
}

func TestMarkdownHeadings(t *testing.T) {
	for _, c := range headingCases {
		for _, w := range []int{headingWindow, 8} {
			hs, err := findHeadings(context.Background(), c.md, w)
			if got := headingStrings(hs); err != nil || strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("%s (window %d): %q %v, want %q", c.name, w, got, err, c.want)
			}
		}
	}
}

// Sections hold the raw Markdown between headings, without the heading lines (a setext underline
// included); text before the first heading is a section of its own.
func TestMarkdownSections(t *testing.T) {
	md := "Intro.\n\n# Setup\nInstall it.\n<!--\n## not a heading\n-->\nline one\nline two\n========\nBody.\n"
	_, secs, err := markdownSections(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	want := []section{
		{path: nil, text: "Intro.\n\n"},
		{path: []string{"Setup"}, text: "Install it.\n<!--\n## not a heading\n-->\n"},
		{path: []string{"line one line two"}, text: "Body.\n"},
	}
	if !reflect.DeepEqual(secs, want) {
		t.Errorf("sections: %q", secs)
	}
}

func TestFrontMatter(t *testing.T) {
	for _, c := range []struct{ md, title, rest string }{
		{"---\ntitle: \"Runbook\"\ntags:\n  - ops\n# a comment\n---\n# A\n", "Runbook", "# A\n"},
		{"---\ntitle: x\n...\nbody", "x", "body"},
		{"---\nSome prose here.\n---\n# A\n", "", "---\nSome prose here.\n---\n# A\n"}, // a rule over prose
		{"---\nSteps to follow: install it\n---\n# A\n", "", "---\nSteps to follow: install it\n---\n# A\n"},
		{"---\n  indented prose\n---\n# A\n", "", "---\n  indented prose\n---\n# A\n"},
		{"---\nNote: read this first.\n---\n# A\n", "", "# A\n"}, // valid YAML, so taken
		{"---\n\"my key\": x\nlist:\n  - a\n- b\n---\nbody", "", "body"},
		{"---\ntitle: x\nno end", "", "---\ntitle: x\nno end"},
		{"# no front matter\n", "", "# no front matter\n"},
	} {
		if title, rest := frontMatter(c.md); title != c.title || rest != c.rest {
			t.Errorf("frontMatter(%q) = %q, %q", c.md, title, rest)
		}
	}
	// The prose under a leading rule is indexed, not dropped as front matter.
	got, err := Chunks(context.Background(), "notes.md", []byte("---\nSome prose here.\n\n---\n# A\nbody\n"))
	if err != nil || len(got) == 0 || !strings.Contains(got[0], "Some prose here.") {
		t.Errorf("a rule over prose: %q %v", got, err)
	}
}

// Lone "\r" line ends (classic Mac OS) read as "\n".
func TestLoneCarriageReturns(t *testing.T) {
	ctx := context.Background()
	mac, err := Chunks(ctx, "notes.md", []byte("# Install\rRun it.\r\r## macOS\rUse brew.\r"))
	unix, err2 := Chunks(ctx, "notes.md", []byte("# Install\nRun it.\n\n## macOS\nUse brew.\n"))
	if err != nil || err2 != nil || !reflect.DeepEqual(mac, unix) || len(mac) != 2 {
		t.Errorf("lone \\r: %q %v, want %q %v", mac, err, unix, err2)
	}
	if got, err := Chunks(ctx, "a.txt", []byte("one\rtwo")); err != nil || len(got) != 1 || got[0] != "one\ntwo" {
		t.Errorf("a .txt with lone \\r: %q %v", got, err)
	}
}

// Lines nested past maxNesting are cut down to it in place, and headings after them are found.
func TestLimitNesting(t *testing.T) {
	deep := strings.Repeat(">", 100) + " text\n"
	src := []byte(deep + strings.Repeat("- ", 40) + "x\n" + strings.Repeat("- ", 40) + "\n")
	limitNesting(src)
	lines := strings.Split(string(src), "\n")
	if want := strings.Repeat(">", maxNesting) + "x" + strings.Repeat(">", 100-maxNesting-1) + " text"; lines[0] != want {
		t.Errorf("quotes: %q", lines[0])
	}
	if want := strings.Repeat("- ", maxNesting) + "x " + strings.Repeat("- ", 40-maxNesting-1) + "x"; lines[1] != want {
		t.Errorf("list items: %q", lines[1])
	}
	if want := strings.TrimSuffix(strings.Repeat("- ", 40), " ") + " "; lines[2] != want { // a rule
		t.Errorf("a thematic break changed: %q", lines[2])
	}
	md := deep + "\n## H\n"
	_, secs, err := markdownSections(context.Background(), md)
	if err != nil || len(secs) != 2 || secs[0].text != deep+"\n" || !reflect.DeepEqual(secs[1].path, []string{"H"}) {
		t.Errorf("after a deep quote: %q %v", secs, err)
	}
}

// A line paragraphOnly accepts can only be paragraph text; a window filled by one paragraph restarts
// at such a line.
func TestParagraphOnly(t *testing.T) {
	for line, want := range map[string]bool{
		"plain text\n": true, "#tag": true, "####### seven": true, "*draft* line": true, "*a_": true,
		"-x": true, "+x": true, "=x": true, "_x": true, "``x": true, "~x": true, "   indented 3": true,
		"# heading": false, "##": false, "- item": false, "-": false, "1. item": false, "2) item": false,
		"***": false, "- - -": false, "___": false, "==": false, "--": false, "```x": false, "~~~": false,
		"> quote": false, "<div>": false, "\tcode": false, "    code": false, "": false, "  ": false,
	} {
		if got := paragraphOnly([]byte(line)); got != want {
			t.Errorf("paragraphOnly(%q) = %v", line, got)
		}
	}
	for _, unit := range []string{"*a_ word\n", "#x word\n", "-x\n=y\n``z\n"} {
		md := strings.Repeat(unit, 1000)
		src := []byte(md[:4096])
		doc := mdParser.Parse(gmtext.NewReader(src))
		if off, inPara := restartInside(doc.FirstChild(), src); off <= 0 || !inPara {
			t.Errorf("a paragraph of %q has no restart point", unit)
		}
	}
}

// The heading finder stops when its context ends.
func TestMarkdownStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := findHeadings(ctx, "# A\n", headingWindow); !errors.Is(err, context.Canceled) {
		t.Errorf("findHeadings on a canceled context: %v", err)
	}
	if _, err := Chunks(ctx, "a.md", []byte("# A\nbody\n")); !errors.Is(err, context.Canceled) {
		t.Errorf("Chunks on a canceled context: %v", err)
	}
}

// Hostile shapes take time linear in their size: 8 times the bytes take well under 16 times as
// long (64 times, for a quadratic shape). A ratio, so a slow machine doesn't fail it.
func TestMarkdownHostileShapesLinear(t *testing.T) {
	repeat := func(s string) func(int) string {
		return func(n int) string { return strings.Repeat(s, n/len(s)) }
	}
	shapes := map[string]func(int) string{
		"quotes on one line":         repeat(">"),
		"spaced quotes on one line":  repeat("> "),
		"list items on one line":     repeat("- "),
		"ordered items on one line":  repeat("1. "),
		"quotes and items on a line": repeat("> - "),
		"unclosed fence":             func(n int) string { return "```\n" + repeat("# x\n")(n) },
		"unclosed comment":           func(n int) string { return "<!--\n" + repeat("# x\n")(n) },
		"brackets":                   repeat("["),
		"emphasis":                   repeat("*a_"),
		"one long line":              repeat("word "),
		"one-line headings":          repeat("# h\n"),
		"quote and list lines":       repeat("> a\n- b\n"),
		"staircase":                  staircase,
	}
	for name, shape := range shapes {
		small, large := shape(64<<10), shape(512<<10)
		ts, tl := fastest(small), fastest(large)
		if tl > 16*ts && tl > 50*time.Millisecond {
			t.Errorf("%s: %v for 64 KiB, %v for 512 KiB", name, ts, tl)
		}
	}
}

// staircase is list items each indented one level deeper than the last, about n bytes of them.
func staircase(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i = (i + 1) % 200 {
		b.WriteString(strings.Repeat("  ", i) + "- item\n")
	}
	return b.String()
}

// fastest is the least of three runs' times finding md's sections.
func fastest(md string) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		_, _, _ = markdownSections(context.Background(), md)
		best = min(best, time.Since(start))
	}
	return best
}

// Parsing a window at a time finds the headings a parse of the whole text finds.
func FuzzMarkdownHeadings(f *testing.F) {
	for _, c := range headingCases {
		f.Add(c.md, uint8(0))
	}
	f.Add(strings.Repeat("para line\n", 20)+"===\n# A\n", uint8(5))
	f.Add("- a\n- b\n\n  c\n- d\n# H\n", uint8(3))
	f.Add("> a\n> b\nlazy\n> c\n## H\n", uint8(1))
	f.Add(strings.Repeat("*a_\n#x\n-x\n=x\n``x\n", 8)+"===\n# A\n", uint8(2))
	f.Fuzz(func(t *testing.T, md string, w uint8) {
		window := 8 + int(w)
		whole, err := findHeadings(context.Background(), md, len(md)+1)
		if err != nil {
			t.Fatal(err)
		}
		windowed, err := findHeadings(context.Background(), md, window)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(windowed, whole) {
			t.Errorf("window %d: %q\nwhole: %q", window, headingStrings(windowed), headingStrings(whole))
		}
	})
}
