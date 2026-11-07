package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// openPTY opens a pseudo-terminal: what's written to the first file is typed at the second.
func openPTY(t *testing.T) (typing, term *os.File) {
	t.Helper()
	return openPTYTo(t, io.Discard)
}

// openPTYTo is openPTY, with what the terminal shows (what's written to it, and its echo) copied
// to shown.
func openPTYTo(t *testing.T, shown io.Writer) (typing, term *os.File) {
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
	// What the terminal shows is read (and dropped, unless shown is set), so it never fills up.
	go func() { _, _ = io.Copy(shown, m) }()
	return m, s
}

// On a real terminal, meko cloud-link's picker takes a number at once and swallows the Enter typed
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
				i, perr := pickKey(term, io.Discard, "How?", signInChoices[:], nil)
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
	if _, err = pickKey(r, &out, "How?", signInChoices[:], nil); err == nil || out.Len() != 0 {
		t.Fatalf("pickKey on a pipe: %v, printed %q", err, out.String())
	}
	if !errors.Is(err, errNoKeyMode) {
		t.Errorf("pickKey on a pipe: %v, want errNoKeyMode", err)
	}
}

// A first install's picker asks on the terminal which experience to add, by the meko command that
// runs it, with no numbers: the mark skips the coming-soon rows, skip for now and Ctrl+C add none,
// and one whose install fails is a ✗ row, with Meko left as it is. On an 80-column terminal a long
// detail is cut to fit its line. Present, which has no Summary, isn't offered; nor is Kode once
// added, which leaves nothing to pick, so the picker isn't shown.
func TestPickExtensionOnATerminal(t *testing.T) {
	dir, _ := setup(t)
	noRealExtensions(t)
	l := paths.New(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	all := []string{"  ? Choose your agentic experience\n",
		"  ❯ kode           agentic development — reviews, PRs, refactors\033[K\n",
		"    migrate        database migrations with a safety net · coming soon\033[K\n",
		"    scribe         briefs, decks, docs and posts from your context · coming soon\033[K\n",
		"    skip for now   explore the base platform — add an experience anytime\033[K\n"}
	// pick runs the picker on a pseudo-terminal cols wide, types each of typed after a pause, and
	// returns what it added and what the terminal showed.
	pick := func(t *testing.T, cols uint16, typed ...string) (string, string) {
		t.Helper()
		var shown syncWriter
		typing, term := openPTYTo(t, &shown)
		if err := unix.IoctlSetWinsize(int(term.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: cols}); err != nil {
			t.Fatal(err)
		}
		old := openTTY
		openTTY = func() (*os.File, error) { return os.OpenFile(term.Name(), os.O_RDWR, 0) }
		t.Cleanup(func() { openTTY = old })
		t.Setenv("NO_COLOR", "1")
		got := make(chan string, 1)
		go func() { got <- pickExtension(l, true, term, term)(newReport(term)) }()
		for _, k := range typed {
			time.Sleep(100 * time.Millisecond)
			if _, err := typing.WriteString(k); err != nil {
				t.Fatal(err)
			}
		}
		name := <-got
		time.Sleep(50 * time.Millisecond)
		return name, strings.ReplaceAll(shown.String(), "\r\n", "\n")
	}
	for _, c := range []struct {
		name  string
		cols  uint16
		typed []string
		shows []string
	}{
		{"down skips the soon rows to skip for now", 120, []string{"\x1b[B", "\r"},
			append(all, "  ❯ skip for now   explore the base platform")},
		{"j moves down too", 120, []string{"j", "\r"}, []string{"  ❯ skip for now"}},
		{"up wraps to skip for now", 120, []string{"k", "\r"}, []string{"  ❯ skip for now"}},
		{"a number does nothing", 120, []string{"2", "\x03"}, nil},
		{"Ctrl+C adds none", 120, []string{"\x03"}, nil},
		{"kode, whose install fails", 120, []string{"\r"}, []string{
			"\n  → meko extension install kode   · adds an extension at any time\n",
			"  ✗ kode            not added: "}},
		{"80 columns cut a long detail", 80, []string{"\x03"}, []string{
			"    scribe         briefs, decks, docs and posts from your conte… · coming soon\033[K\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			name, out := pick(t, c.cols, c.typed...)
			if name != "" {
				t.Errorf("added %q", name)
			}
			for _, s := range c.shows {
				if !strings.Contains(out, s) {
					t.Errorf("no %q in:\n%s", s, out)
				}
			}
			if strings.Contains(out, "1)") || strings.Contains(out, "Enter") {
				t.Errorf("numbers or a hint in:\n%s", out)
			}
			if strings.Contains(out, "present") {
				t.Errorf("Present offered in:\n%s", out)
			}
		})
	}

	reg, err := extensions.Load(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	reg.Entries["kode"] = extensions.Entry{Command: "/bin/true"}
	if err = reg.Save(); err != nil {
		t.Fatal(err)
	}
	if name, out := pick(t, 120); name != "" || out != "" {
		t.Errorf("with Kode added: added %q, showed:\n%s", name, out)
	}
}

// syncWriter is a strings.Builder the pseudo-terminal's reader writes to while the test reads it.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// When ~/.local/bin isn't on the PATH, install asks on the terminal whether to add it to the shell's
// profile: yes or Enter adds the line and says how to use it now, no names the line to add. Off a
// terminal, for a shell other than zsh or bash, or with --non-interactive (ask false), it asks
// nothing and names the line.
func TestInstallRowOffersThePath(t *testing.T) {
	line := `export PATH="$HOME/.local/bin:$PATH"`
	for _, c := range []struct {
		name, shell, typed string
		ask, added         bool
		shows              []string
	}{
		{"yes", "/bin/bash", "y\n", true, true, []string{
			"  ! Install         meko in ~/.local/bin · not on your PATH\n",
			"  ? Add ~/.local/bin to your PATH in ~/.bashrc? [Y/n] ",
			"  ✓ PATH            added to ~/.bashrc · open a new terminal, or run . ~/.bashrc\n"}},
		{"Enter", "/bin/bash", "\n", true, true, []string{"  ✓ PATH            added to ~/.bashrc"}},
		{"no", "/bin/bash", "n\n", true, false, []string{
			"    Add " + line + " to your shell's profile to run meko by its name\n"}},
		{"zsh", "/usr/bin/zsh", "Y\n", true, false, []string{"in ~/.zshrc? [Y/n] "}},
		{"fish", "/usr/bin/fish", "", true, false, []string{
			"  ! Install         meko in ~/.local/bin · not on your PATH: add " + line + " to your shell's profile\n"}},
		{"--non-interactive", "/bin/bash", "", false, false, []string{"not on your PATH: add " + line}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", c.shell)
			t.Setenv("PATH", "/usr/bin:/bin")
			t.Setenv("NO_COLOR", "1")
			var shown syncWriter
			typing, term := openPTYTo(t, &shown)
			old := openTTY
			openTTY = func() (*os.File, error) { return os.OpenFile(term.Name(), os.O_RDWR, 0) }
			defer func() { openTTY = old }()
			done := make(chan struct{})
			go func() {
				installRow(newReport(term), home+"/.local/bin", home, c.ask)
				close(done)
			}()
			if c.typed != "" {
				time.Sleep(100 * time.Millisecond)
				if _, err := typing.WriteString(c.typed); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			time.Sleep(50 * time.Millisecond)
			out := strings.ReplaceAll(shown.String(), "\r\n", "\n")
			for _, s := range c.shows {
				if !strings.Contains(out, s) {
					t.Errorf("no %q in:\n%s", s, out)
				}
			}
			profile := home + "/.bashrc"
			if c.shell == "/usr/bin/zsh" {
				profile = home + "/.zshrc"
			}
			b, _ := os.ReadFile(profile) //nolint:gosec // G304: the test's own file
			if want := c.added || c.shell == "/usr/bin/zsh"; strings.Contains(string(b), line) != want {
				t.Errorf("%s holds %q, want the line: %v", profile, b, want)
			}
			if !c.ask && strings.Contains(out, "[Y/n]") {
				t.Errorf("asked with ask false:\n%s", out)
			}
		})
	}
}
