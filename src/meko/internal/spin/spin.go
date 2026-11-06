// Package spin prints the CLI's summary rows ("  ✓ Server          running"), and the rotating dots a
// row shows on a terminal while its step runs: gh's spinner, blue, the one install.sh's download shows.
//
// It holds the CLI's palette, which install.sh repeats (docs/install.md, "Colors"). Each role is a
// base ANSI color, which every terminal theme tunes for its own background, so the rows read on a
// white terminal as on a black one: blue (34) marks, commands, links and dots; green (32) ✓ and ●;
// bold yellow (1;33) !; red (31) ✗; faint (2) notes; bold (1) in the terminal's own text color for
// emphasis. White (37, 97) and the bright colors (90–97) are never text on the terminal's own
// background, since a light theme draws them pale or not at all: only the call to action, which sets
// its own blue background, is white (1;97;44).
package spin

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// Kind is how a step went: done, a warning, or failed.
type Kind int

const (
	OK Kind = iota
	Warn
	Fail
)

var (
	marks  = [...]string{"✓", "!", "✗"}
	colors = [...]string{green, "\033[1;33m", "\033[31m"}
)

// The palette's codes.
const (
	reset = "\033[0m"
	blue  = "\033[34m"
	green = "\033[32m"
	bold  = "\033[1m"
	faint = "\033[2m"
	cta   = "\033[1;97;44m"
	link  = "\033[4;34m"
)

// Frames and Interval are the dots' animation: gh's.
var (
	Frames   = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}
	Interval = 120 * time.Millisecond
)

// frame is the dots' i-th frame, blue when color is set.
func frame(i int, color bool) string { return Blue(Frames[i%len(Frames)], color) }

func paint(code, s string, color bool) string {
	if !color || s == "" {
		return s
	}
	return code + s + reset
}

// Blue is s as a mark (◆ ? ❯ →) or a command in a hint.
func Blue(s string, color bool) string { return paint(blue, s, color) }

// Green is s as done: ✓ and the ● of a headline.
func Green(s string, color bool) string { return paint(green, s, color) }

// Bold is s emphasized, in the terminal's own text color.
func Bold(s string, color bool) string { return paint(bold, s, color) }

// Dim is s as a note: secondary text, after a row's value.
func Dim(s string, color bool) string { return paint(faint, s, color) }

// Note is text followed by note, dim, after " · ": "~/.meko · secrets in your login Keychain". An
// empty note leaves text as it is.
func Note(text, note string, color bool) string {
	if note == "" {
		return text
	}
	return text + Dim(" · "+note, color)
}

// Link is url blue and underlined, and an OSC 8 hyperlink, which terminals that know it make
// clickable (and others ignore).
func Link(url string, color bool) string {
	if !color {
		return url
	}
	return "\033]8;;" + url + "\033\\" + link + url + reset + "\033]8;;\033\\"
}

// CTA is s as the call to action, white on blue with a space on each side; without color, s alone.
func CTA(s string, color bool) string {
	if !color {
		return s
	}
	return cta + " " + s + " " + reset
}

// animates reports whether w shows the dots: a terminal that draws them; tests replace it.
var animates = func(w io.Writer) bool { return IsTerminal(w) && os.Getenv("TERM") != "dumb" }

// IsTerminal reports whether v (a reader or writer) is a terminal (a character device): one that
// echoes what's typed, or shows colors.
func IsTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&fs.ModeCharDevice != 0
}

// Color reports whether rows on w are colored: w is a terminal, NO_COLOR is unset and TERM isn't dumb.
func Color(w io.Writer) bool {
	return IsTerminal(w) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// Mark is k's mark, colored when color is set.
func Mark(k Kind, color bool) string {
	if !color {
		return marks[k]
	}
	return colors[k] + marks[k] + reset
}

// Row prints one row: "  ✓ Server          running".
func Row(w io.Writer, k Kind, color bool, label, text string) {
	fmt.Fprintf(w, "  %s %-15s %s\n", Mark(k, color), label, text)
}

// Step is a step that takes a while. On a terminal its row shows the dots until it ends; anywhere
// else (a log, or the Extensions page's job output) a step with a row says what it's doing as a plain
// line when it starts, so a long download doesn't leave the output empty. Only its first Done, Fail
// or Clear counts.
type Step struct {
	w            io.Writer
	label, doing string
	stop         chan struct{}
	stopped      chan struct{}
	once         sync.Once
}

// Start begins a step whose row is label: "  ⣾ Release         installing meko 1.2.4", with no "…" after
// it since the dots already say it's under way.
func Start(w io.Writer, label, doing string) *Step {
	s := &Step{w: w, label: label, doing: doing}
	switch {
	case animates(w):
		s.run()
	case label != "" && doing != "":
		fmt.Fprintf(w, "%s%s\n", strings.ToUpper(doing[:1]), doing[1:])
	}
	return s
}

// Quiet begins a step whose row shows the dots on a terminal, as Start's does, but that prints
// nothing anywhere else until it ends: for a short wait, whose row says the rest ("ready in 2.4s").
func Quiet(w io.Writer, label, doing string) *Step {
	s := &Step{w: w, label: label, doing: doing}
	if animates(w) {
		s.run()
	}
	return s
}

// Wait begins a step with no row of its own, "  ⣾ Looking for releases…", which Clear erases: for
// one whose outcome the next line says. Off a terminal it prints nothing.
func Wait(w io.Writer, doing string) *Step { return Start(w, "", doing) }

func (s *Step) run() {
	s.stop, s.stopped = make(chan struct{}), make(chan struct{})
	color := Color(s.w)
	go func() {
		defer close(s.stopped)
		t := time.NewTicker(Interval)
		defer t.Stop()
		for i := 0; ; i++ {
			if s.label == "" {
				fmt.Fprintf(s.w, "\r  %s %s…\033[K", frame(i, color), s.doing)
			} else {
				fmt.Fprintf(s.w, "\r  %s %-15s %s\033[K", frame(i, color), s.label, s.doing)
			}
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
}

// end stops the dots and erases their line, then prints row, the first time only.
func (s *Step) end(row func()) {
	s.once.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.stopped
			fmt.Fprint(s.w, "\r\033[K")
		}
		row()
	})
}

// End ends the step with its row marked k, saying text.
func (s *Step) End(k Kind, text string) {
	s.end(func() { Row(s.w, k, Color(s.w), s.label, text) })
}

// Done ends the step with its ✓ row, saying text.
func (s *Step) Done(text string) { s.End(OK, text) }

// Fail ends the step with its ✗ row, saying what it was doing; the caller prints the error.
func (s *Step) Fail() {
	s.end(func() { Row(s.w, Fail, Color(s.w), s.label, s.doing) })
}

// Clear ends the step and leaves nothing of it.
func (s *Step) Clear() { s.end(func() {}) }
