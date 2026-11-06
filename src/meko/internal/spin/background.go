package spin

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The blue of marks, commands, links and dots is the design's: a light blue (#79aeff) on a dark
// background, a deeper one (#1f5fd1) on a light one. Which one depends on the terminal's
// background, which Background finds. Without 24-bit color (COLORTERM truecolor or 24bit, which SSH
// usually doesn't pass on) it's the nearest of the 256 colors (111 #87afff, 26 #005fd7), which
// nearly every terminal has, whatever its TERM says. Only a terminal with just the 16 base colors
// (sixteenColors) gets the theme's own bright blue (94) on dark and blue (34) on light, which some
// themes draw purple.
var blues = map[string][3]string{ // background: 24-bit, 256-color, 16-color
	"dark":  {"38;2;121;174;255", "38;5;111", "94"},
	"light": {"38;2;31;95;209", "38;5;26", "34"},
}

// BackgroundEnv names the terminal's background, dark or light, when it's known: install.sh sets it
// for the meko install it runs, which passes it on to the extensions' installs, and a person may set
// it to override what's found.
const BackgroundEnv = "MEKO_TERM_BG"

var (
	blueOnce sync.Once
	blueSGR  string
)

// blueCode is the escape sequence for the palette's blue on this terminal, found once.
func blueCode() string {
	blueOnce.Do(func() {
		depth := 1
		switch {
		case os.Getenv("COLORTERM") == "truecolor" || os.Getenv("COLORTERM") == "24bit":
			depth = 0
		case sixteenColors(os.Getenv("TERM")):
			depth = 2
		}
		blueSGR = "\033[" + blues[Background()][depth] + "m"
	})
	return blueSGR
}

// sixteenColors reports whether a terminal of type term has only the 16 base colors: the Linux and
// BSD consoles and the old hardware terminals.
func sixteenColors(term string) bool {
	for _, p := range []string{"linux", "vt", "ansi", "cons25", "sun"} {
		if strings.HasPrefix(term, p) {
			return true
		}
	}
	return false
}

var (
	bgOnce sync.Once
	bg     string
)

// Background is the terminal's background, "dark" or "light": BackgroundEnv when it's set, else
// COLORFGBG (set by some terminals: its last field is the background's color number), else what the
// terminal answers when asked for its background color (queryBackground), else dark. It sets
// BackgroundEnv to what it found, so the commands meko runs (an extension's install) use the same
// blue without asking the terminal again.
func Background() string {
	bgOnce.Do(func() {
		bg = backgroundFrom(os.Getenv(BackgroundEnv), os.Getenv("COLORFGBG"), queryBackground)
		_ = os.Setenv(BackgroundEnv, bg)
	})
	return bg
}

// backgroundFrom is Background's rule, with what it reads passed in.
func backgroundFrom(env, colorfgbg string, query func() (string, bool)) string {
	if env == "dark" || env == "light" {
		return env
	}
	if i := strings.LastIndex(colorfgbg, ";"); i >= 0 {
		if n, err := strconv.Atoi(colorfgbg[i+1:]); err == nil {
			if n == 7 || (n >= 9 && n <= 15) {
				return "light"
			}
			return "dark"
		}
	}
	if b, ok := query(); ok {
		return b
	}
	return "dark"
}

// queryBackground asks the terminal for its background color (OSC 11), then for its attributes
// (DA1), which every terminal answers: so it reads until that answer, or a quarter of a second,
// and never leaves a late reply to show up as typing. It asks only when stdout is a terminal, on
// /dev/tty, with echo off. tests replace it.
var queryBackground = func() (string, bool) {
	if !IsTerminal(os.Stdout) || os.Getenv("TERM") == "dumb" {
		return "", false
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", false
	}
	raw := *old
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 0, 1
	if err = unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return "", false
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }()
	if _, err = f.WriteString("\033]11;?\033\\\033[c"); err != nil {
		return "", false
	}
	var reply []byte
	buf := make([]byte, 64)
	for deadline := time.Now().Add(250 * time.Millisecond); time.Now().Before(deadline); {
		n, _ := f.Read(buf)
		reply = append(reply, buf[:n]...)
		if da1.Match(reply) {
			break
		}
	}
	return backgroundOf(reply)
}

var (
	da1   = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)
	osc11 = regexp.MustCompile(`\]11;rgba?:([0-9a-fA-F]+)/([0-9a-fA-F]+)/([0-9a-fA-F]+)`)
)

// backgroundOf reads a terminal's OSC 11 reply ("\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\"): dark when the
// color's luminance is under half.
func backgroundOf(reply []byte) (string, bool) {
	m := osc11.FindSubmatch(reply)
	if m == nil {
		return "", false
	}
	var c [3]float64
	for i, h := range m[1:4] {
		v, err := strconv.ParseUint(string(h), 16, 32)
		if err != nil {
			return "", false
		}
		c[i] = float64(v) / float64(uint64(1)<<(4*len(h))-1)
	}
	if 0.299*c[0]+0.587*c[1]+0.114*c[2] < 0.5 {
		return "dark", true
	}
	return "light", true
}
