package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: what's written to the first file is typed at the second.
func openPTY(t *testing.T) (typing, term *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err = unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// What the terminal echoes back is read and dropped, so the echo never fills up.
	go func() { _, _ = io.Copy(io.Discard, m) }()
	return m, s
}

// On a real terminal, meko cloud_link's picker takes a number at once and swallows the Enter typed
// after it out of habit, so the key prompt next reads what's typed for it, not an empty line; the
// arrows and Enter pick too, and the terminal is left as it was.
func TestPickKeyOnATerminal(t *testing.T) {
	for _, c := range []struct {
		name  string
		typed []string // each typed after a pause
		want  int
	}{
		{"2, then Enter out of habit", []string{"2", "\r"}, 1},
		{"2 alone", []string{"2"}, 1},
		{"2 and Enter at once", []string{"2\r"}, 1},
		{"down, then Enter", []string{"\x1b[B", "\r"}, 1},
		{"Enter picks the first", []string{"\r"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			typing, term := openPTY(t)
			before, err := unix.IoctlGetTermios(int(term.Fd()), ioctlGetTermios)
			if err != nil {
				t.Fatal(err)
			}
			got := make(chan int, 1)
			errs := make(chan error, 1)
			go func() {
				i, perr := pickKey(term, io.Discard, "How?", signInChoices[:])
				got <- i
				errs <- perr
			}()
			for _, k := range c.typed {
				time.Sleep(50 * time.Millisecond)
				if _, err = typing.WriteString(k); err != nil {
					t.Fatal(err)
				}
			}
			if i := <-got; i != c.want {
				t.Errorf("picked %d, want %d", i, c.want)
			}
			if err = <-errs; err != nil {
				t.Fatal(err)
			}
			after, err := unix.IoctlGetTermios(int(term.Fd()), ioctlGetTermios)
			if err != nil || after.Lflag != before.Lflag {
				t.Errorf("the terminal's settings after: %+v %v, before %+v", after, err, before)
			}
			if _, err = typing.WriteString("the-key\n"); err != nil {
				t.Fatal(err)
			}
			line, err := newAnswerReader(term).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "the-key" {
				t.Errorf("the next prompt read %q, %v", line, err)
			}
		})
	}
}

// A file that isn't a terminal can't be read a key at a time: pickKey says so before printing
// anything, for chooseSignIn to ask for a line instead.
func TestPickKeyNeedsATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	var out strings.Builder
	if _, err = pickKey(r, &out, "How?", signInChoices[:]); err == nil || out.Len() != 0 {
		t.Fatalf("pickKey on a pipe: %v, printed %q", err, out.String())
	}
	if !errors.Is(err, errNoKeyMode) {
		t.Errorf("pickKey on a pipe: %v, want errNoKeyMode", err)
	}
}
