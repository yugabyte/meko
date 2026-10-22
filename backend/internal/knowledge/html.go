package knowledge

import (
	"errors"
	"io"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Elements whose content isn't the page's text.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Svg: true,
	atom.Head: true, atom.Iframe: true, atom.Object: true,
}

// Elements that end a paragraph, and those that end a line.
var (
	paragraphs = map[atom.Atom]bool{
		atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Blockquote: true,
		atom.Table: true, atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Header: true, atom.Footer: true,
		atom.Main: true, atom.Aside: true, atom.Nav: true, atom.Figure: true, atom.Form: true, atom.Pre: true,
		atom.Hr: true,
	}
	lines = map[atom.Atom]bool{
		atom.Br: true, atom.Li: true, atom.Tr: true, atom.Dt: true, atom.Dd: true, atom.Figcaption: true,
		atom.Caption: true,
	}
	headingLevel = map[atom.Atom]int{atom.H1: 1, atom.H2: 2, atom.H3: 3, atom.H4: 4, atom.H5: 5, atom.H6: 6}
)

var (
	blankLines    = regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`)
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
)

// tidy drops the spaces before line breaks and the extra blank lines that nested block elements
// leave.
func tidy(s string) string {
	return blankLines.ReplaceAllString(trailingSpace.ReplaceAllString(s, "\n"), "\n\n")
}

// htmlSections is an HTML page's text in sections at its h1 to h6 headings, without scripts,
// styles and the like, and its title. Block elements become paragraph and line breaks.
func htmlSections(text string) (title string, sections []section, err error) {
	z := html.NewTokenizer(strings.NewReader(text))
	var (
		h        headings
		cur      section
		body     strings.Builder
		heading  strings.Builder
		inTitle  bool
		titleBuf strings.Builder
		level    int // the heading being read, 0 outside one
		skip     int // depth inside skipped elements
		pre      int // depth inside pre
	)
	flush := func(next []string) {
		cur.text = tidy(body.String())
		sections = append(sections, cur)
		cur = section{path: next}
		body.Reset()
	}
	write := func(s string) {
		if level > 0 {
			heading.WriteString(s)
		} else {
			body.WriteString(s)
		}
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				flush(nil)
				return strings.Join(strings.Fields(titleBuf.String()), " "), sections, nil
			}
			return "", nil, invalid("the HTML couldn't be read")
		case html.TextToken:
			t := string(z.Text())
			switch {
			case inTitle:
				titleBuf.WriteString(t)
			case skip > 0:
			case pre > 0:
				write(t)
			default:
				if f := strings.Join(strings.Fields(t), " "); f != "" {
					if strings.TrimLeft(t, " \t\r\n") != t {
						f = " " + f
					}
					if strings.TrimRight(t, " \t\r\n") != t {
						f += " "
					}
					write(f)
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			end := tt == html.EndTagToken
			if a == atom.Title {
				inTitle = !end && tt != html.SelfClosingTagToken
				continue
			}
			if skipped[a] {
				if tt == html.StartTagToken {
					skip++
				} else if end && skip > 0 {
					skip--
				}
				continue
			}
			if skip > 0 {
				continue
			}
			if a == atom.Pre {
				if tt == html.StartTagToken {
					pre++
				} else if end && pre > 0 {
					pre--
				}
			}
			if l, ok := headingLevel[a]; ok {
				if !end {
					level = l
					heading.Reset()
				} else if level > 0 {
					text := strings.Join(strings.Fields(heading.String()), " ")
					level = 0
					if text != "" {
						flush(h.enter(l, text))
					}
				}
				continue
			}
			switch {
			case paragraphs[a]:
				write("\n\n")
			case lines[a] && !end: // a line starts at each item, row and break
				write("\n")
			case (a == atom.Td || a == atom.Th) && end:
				write(" ")
			}
		}
	}
}
