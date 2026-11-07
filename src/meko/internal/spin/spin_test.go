package spin

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Off a terminal a step with a row says what it's doing as a plain line when it starts, then Done
// and Fail print its row; a Wait leaves nothing.
func TestStepWithoutATerminal(t *testing.T) {
	var b bytes.Buffer
	s := Start(&b, "Release", "installing meko 1.2.4")
	if want := "Installing meko 1.2.4\n"; b.String() != want {
		t.Fatalf("Start printed %q, want %q", b.String(), want)
	}
	s.Done("meko 1.2.4 installed")
	if want := "Installing meko 1.2.4\n  ✓ Release         meko 1.2.4 installed\n"; b.String() != want {
		t.Errorf("Done printed %q, want %q", b.String(), want)
	}

	b.Reset()
	Start(&b, "Release", "installing meko 1.2.4").Fail()
	if want := "Installing meko 1.2.4\n  ✗ Release         installing meko 1.2.4\n"; b.String() != want {
		t.Errorf("Fail printed %q, want %q", b.String(), want)
	}

	b.Reset()
	Wait(&b, "Looking for releases").Clear()
	if b.Len() != 0 {
		t.Errorf("Wait left %q", b.String())
	}
}

// On a terminal the row shows the rotating dots, each frame drawn over the last, until the step
// ends: then the line is erased and the row printed in its place.
func TestStepAnimates(t *testing.T) {
	forceAnimation(t)
	var b syncBuffer
	s := Start(&b, "Release", "installing meko 1.2.4")
	waitFor(t, &b, Frames[1])
	s.Done("meko 1.2.4 installed")
	out := b.String()
	if !strings.HasPrefix(out, "\r  "+Frames[0]+" Release         installing meko 1.2.4\033[K") {
		t.Errorf("first frame: %q", out)
	}
	if !strings.HasSuffix(out, "\r\033[K  ✓ Release         meko 1.2.4 installed\n") {
		t.Errorf("end: %q", out)
	}

	b.Reset()
	w := Wait(&b, "Looking for releases")
	waitFor(t, &b, Frames[1])
	w.Clear()
	out = b.String()
	if !strings.HasPrefix(out, "\r  "+Frames[0]+" Looking for releases…\033[K") ||
		!strings.HasSuffix(out, "\033[K\r\033[K") {
		t.Errorf("Wait drew %q", out)
	}
}

// useBlue makes the palette's blue code for the test, as found once on a terminal.
func useBlue(t *testing.T, code string) {
	t.Helper()
	blueOnce.Do(func() {})
	old := blueSGR
	blueSGR = "\033[" + code + "m"
	t.Cleanup(func() { blueSGR = old })
}

// The dots are blue where rows are colored, and plain where they aren't.
func TestFrameColor(t *testing.T) {
	useBlue(t, "38;2;121;174;255")
	if got, want := frame(0, true), "\033[38;2;121;174;255m⣾\033[0m"; got != want {
		t.Errorf("frame(0, true) = %q, want %q", got, want)
	}
	if got := frame(len(Frames)+1, false); got != Frames[1] {
		t.Errorf("frame(%d, false) = %q, want %q", len(Frames)+1, got, Frames[1])
	}
}

// Each role of the palette has its code when color is on, and none when it's off.
func TestPalette(t *testing.T) {
	useBlue(t, "38;2;121;174;255")
	for _, c := range []struct{ got, want string }{
		{Mark(OK, true), "\033[32m✓\033[0m"},
		{Mark(Warn, true), "\033[1;33m!\033[0m"},
		{Mark(Fail, true), "\033[31m✗\033[0m"},
		{Blue("meko stop", true), "\033[38;2;121;174;255mmeko stop\033[0m"},
		{Green("●", true), "\033[32m●\033[0m"},
		{Bold("meko", true), "\033[1mmeko\033[0m"},
		{Dim("no sudo", true), "\033[2mno sudo\033[0m"},
		{Note("~/.meko", "kept", true), "~/.meko\033[2m · kept\033[0m"},
		{Note("~/.meko", "", true), "~/.meko"},
		{CTA("meko ui", true), "\033[1;97;44m meko ui \033[0m"},
		{Link("http://127.0.0.1:7777", true),
			"\033]8;;http://127.0.0.1:7777\033\\\033[4m\033[38;2;121;174;255mhttp://127.0.0.1:7777\033[0m\033]8;;\033\\"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	for _, got := range []string{Mark(OK, false), Blue("x", false), Green("x", false), Bold("x", false),
		Dim("x", false), Note("x", "y", false), CTA("x", false), Link("x", false)} {
		if strings.Contains(got, "\033") {
			t.Errorf("%q has an escape code with color off", got)
		}
	}
}

// Color is off for a writer that isn't a terminal, and with NO_COLOR or TERM=dumb.
func TestColorNeedsATerminal(t *testing.T) {
	if Color(&bytes.Buffer{}) {
		t.Error("color on a buffer")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no terminal: %v", err)
	}
	defer func() { _ = tty.Close() }()
	t.Setenv("TERM", "dumb")
	if Color(tty) {
		t.Error("color with TERM=dumb")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	if Color(tty) {
		t.Error("color with NO_COLOR")
	}
}

// No text is white (37, 97) or a bright color (90–97) on the terminal's own background, in this
// package or in install.sh, which share the palette: a light theme draws those pale or not at all.
// The call to action (1;97;44) sets its own background, so it may; the blues are picked for the
// background (blues, and install.sh's), so 94 there is only ever drawn on a dark one.
func TestPaletteReadsOnWhiteTerminals(t *testing.T) {
	bad := regexp.MustCompile(`\[(?:[0-9;]*;)?(?:37|9[0-7])(?:;[0-9;]*)?m`)
	for _, f := range []string{"spin.go", "../../scripts/install.sh"} {
		b, err := os.ReadFile(f) //nolint:gosec // G304: this repo's files
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range bad.FindAllString(string(b), -1) {
			if m != "[1;97;44m" {
				t.Errorf("%s writes %q, which a white terminal can't show", f, m)
			}
		}
	}
}

// Only a step's first ending counts.
func TestStepEndsOnce(t *testing.T) {
	forceAnimation(t)
	var b syncBuffer
	s := Start(&b, "Release", "installing")
	s.Done("installed")
	s.Fail()
	s.Clear()
	if out := b.String(); strings.Count(out, "✓") != 1 || strings.Contains(out, "✗") ||
		!strings.HasSuffix(out, "\r\033[K  ✓ Release         installed\n") {
		t.Errorf("printed %q", out)
	}
}

func forceAnimation(t *testing.T) {
	t.Helper()
	old, oldInterval := animates, Interval
	animates = func(io.Writer) bool { return true }
	Interval = time.Millisecond
	t.Cleanup(func() { animates, Interval = old, oldInterval })
}

// waitFor waits until b holds s.
func waitFor(t *testing.T, b *syncBuffer, s string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(b.String(), s); {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", s, b.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer the test reads while the step's goroutine writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// The background comes from MEKO_TERM_BG, else COLORFGBG's last field, else the terminal's answer,
// else it's taken as dark.
func TestBackgroundFrom(t *testing.T) {
	never := func() (string, bool) { t.Error("asked the terminal"); return "", false }
	light := func() (string, bool) { return "light", true }
	none := func() (string, bool) { return "", false }
	for _, c := range []struct {
		env, colorfgbg string
		query          func() (string, bool)
		want           string
	}{
		{"light", "15;0", never, "light"},
		{"dark", "", never, "dark"},
		{"", "15;0", never, "dark"},
		{"", "0;15", never, "light"},
		{"", "0;default;7", never, "light"},
		{"", "", light, "light"},
		{"bogus", "", light, "light"},
		{"", "", none, "dark"},
	} {
		if got := backgroundFrom(c.env, c.colorfgbg, c.query); got != c.want {
			t.Errorf("backgroundFrom(%q, %q) = %q, want %q", c.env, c.colorfgbg, got, c.want)
		}
	}
}

// A terminal's OSC 11 reply gives its background color, with 1 to 4 hex digits a channel; a reply
// with only the DA1 answer (a terminal that doesn't know OSC 11) gives none.
func TestBackgroundOf(t *testing.T) {
	for reply, want := range map[string]string{
		"\033]11;rgb:1e1e/1e1e/2e2e\033\\\033[?62;22c": "dark",
		"\033]11;rgb:ffff/ffff/ffff\007\033[?1;2c":     "light",
		"\033]11;rgb:fd/f6/e3\033\\":                   "light",
		"\033]11;rgba:0000/2b2b/3636/ffff\033\\":       "dark",
		"\033[?1;2c":                                   "",
	} {
		got, ok := backgroundOf([]byte(reply))
		if got != want || ok != (want != "") {
			t.Errorf("backgroundOf(%q) = %q, %v, want %q", reply, got, ok, want)
		}
	}
}

// Only the consoles and old hardware terminals are taken to have just 16 colors; any other TERM,
// plain xterm included, gets the 256-color blue.
func TestSixteenColors(t *testing.T) {
	for term, want := range map[string]bool{"linux": true, "vt100": true, "ansi": true, "xterm": false,
		"xterm-256color": false, "screen": false, "tmux-256color": false, "": false} {
		if got := sixteenColors(term); got != want {
			t.Errorf("sixteenColors(%q) = %v, want %v", term, got, want)
		}
	}
}
