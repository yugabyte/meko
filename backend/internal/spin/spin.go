// Package spin prints the CLI's summary rows ("  ✓ Server          running"), and the rotating dots a
// row shows on a terminal while its step runs: gh's spinner, the one install.sh's download shows.
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
	colors = [...]string{"\033[32m", "\033[33m", "\033[31m"}
)

// Frames and Interval are the dots' animation: gh's.
var (
	Frames   = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}
	Interval = 120 * time.Millisecond
)

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

// Color reports whether rows on w are colored: w is a terminal and NO_COLOR is unset.
func Color(w io.Writer) bool { return IsTerminal(w) && os.Getenv("NO_COLOR") == "" }

// Mark is k's mark, colored when color is set.
func Mark(k Kind, color bool) string {
	if !color {
		return marks[k]
	}
	return colors[k] + marks[k] + "\033[0m"
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

// Start begins a step whose row is label: "  ⣾ Release         installing meko 1.2.4…".
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

// Wait begins a step with no row of its own, "  ⣾ Looking for releases…", which Clear erases: for
// one whose outcome the next line says. Off a terminal it prints nothing.
func Wait(w io.Writer, doing string) *Step { return Start(w, "", doing) }

func (s *Step) run() {
	s.stop, s.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.stopped)
		t := time.NewTicker(Interval)
		defer t.Stop()
		for i := 0; ; i++ {
			if s.label == "" {
				fmt.Fprintf(s.w, "\r  %s %s…\033[K", Frames[i%len(Frames)], s.doing)
			} else {
				fmt.Fprintf(s.w, "\r  %s %-15s %s…\033[K", Frames[i%len(Frames)], s.label, s.doing)
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

// Done ends the step with its ✓ row, saying text.
func (s *Step) Done(text string) {
	s.end(func() { Row(s.w, OK, Color(s.w), s.label, text) })
}

// Fail ends the step with its ✗ row, saying what it was doing; the caller prints the error.
func (s *Step) Fail() {
	s.end(func() { Row(s.w, Fail, Color(s.w), s.label, s.doing) })
}

// Clear ends the step and leaves nothing of it.
func (s *Step) Clear() { s.end(func() {}) }
