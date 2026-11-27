// Package callhometest keeps tests from sending reports to YugabyteDB's call home service
// (callhome.DefaultURL). A test binary that can build a sender runs Run in its TestMain: it puts a
// guard in front of http.DefaultTransport that turns away every request to DefaultURL's host, and
// fails the run if anything tried. The meko commands a test runs as the test binary (serve,
// _launch) inherit EnvGuard and write what they turn away to its file, so their reports fail the
// run too. Only tests import it.
package callhometest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
)

// EnvGuard names the file where the test binary's child processes write the reports they turned
// away. The first Guard sets it, and the processes it starts inherit it.
const EnvGuard = "MEKO_TEST_CALL_HOME_GUARD"

// ErrBlocked is what a request to DefaultURL gets instead of an answer.
var ErrBlocked = errors.New("a test sent a call home report to callhome.DefaultURL")

var (
	mu      sync.Mutex
	once    sync.Once
	blocked []string // the requests turned away in this process
	file    string   // EnvGuard's file: the one this process made, or its parent's
	parent  bool     // this process had no EnvGuard: it keeps what it turns away itself
)

// Guard puts the guard in front of http.DefaultTransport, once. A process without EnvGuard (the
// test binary) keeps what it turns away itself, and makes the file its children write theirs to
// and sets EnvGuard; one with it (a child) appends what it turns away to that file. When the file
// can't be made, the test binary's own reports still fail the run, and it says on stderr that its
// children's aren't counted.
func Guard() {
	once.Do(func() {
		file = os.Getenv(EnvGuard)
		if file == "" {
			parent = true
			f, err := os.CreateTemp("", "meko-call-home-guard-*")
			if err != nil {
				fmt.Fprintf(os.Stderr, "callhometest: child processes' call home reports won't be counted: %v\n", err)
			} else {
				file = f.Name()
				_ = f.Close()
				_ = os.Setenv(EnvGuard, file)
			}
		}
		http.DefaultTransport = &guard{next: http.DefaultTransport, host: hostOf(callhome.DefaultURL)}
	})
}

// Run guards the test binary, runs its tests, and returns their exit code, or 1 when any report
// was turned away, here or in a child, listed on stderr. It removes the file it made.
func Run(m *testing.M) int {
	Guard()
	code := m.Run()
	if got := Blocked(); len(got) > 0 {
		fmt.Fprintf(os.Stderr, "Tests sent %d call home reports to callhome.DefaultURL (turned away):\n  %s\n",
			len(got), strings.Join(got, "\n  "))
		code = 1
	}
	if parent && file != "" {
		_ = os.Remove(file)
	}
	return code
}

// Check fails t if a report is turned away while it runs, child processes' too, with what they
// were. Call it before the test's other cleanups, so it runs after them.
func Check(t testing.TB) {
	t.Helper()
	own, children := sources()
	t.Cleanup(func() {
		nowOwn, nowChildren := sources()
		got := append(since(nowOwn, own), since(nowChildren, children)...)
		if len(got) > 0 {
			t.Errorf("sent %d call home reports to callhome.DefaultURL (turned away): point call home at a test "+
				"server (MEKO_CALL_HOME_URL):\n  %s", len(got), strings.Join(got, "\n  "))
		}
	})
}

// Blocked is every request turned away so far: this process's, then its children's.
func Blocked() []string {
	own, children := sources()
	return append(own, children...)
}

// since is what now has after the first len(before) lines, or all of it if it shrank.
func since(now, before []string) []string {
	if len(now) < len(before) {
		return now
	}
	return now[len(before):]
}

// sources is what this process turned away, and what its children did, from the file.
func sources() (own, children []string) {
	mu.Lock()
	own = append([]string(nil), blocked...)
	mu.Unlock()
	if !parent || file == "" {
		return own, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return own, nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			children = append(children, line)
		}
	}
	return own, children
}

// guard turns away requests to host and passes the rest to next.
type guard struct {
	next http.RoundTripper
	host string
}

func (g *guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.EqualFold(r.URL.Hostname(), g.host) {
		return g.next.RoundTrip(r)
	}
	line := describe(r)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if parent {
		mu.Lock()
		blocked = append(blocked, line)
		mu.Unlock()
	} else if err := appendLine(line + " (from pid " + fmt.Sprint(os.Getpid()) + ")"); err != nil {
		fmt.Fprintf(os.Stderr, "callhometest: turned away %s, and can't tell the test binary: %v\n", line, err)
	}
	return nil, ErrBlocked
}

// appendLine adds line to EnvGuard's file, for the test binary to read.
func appendLine(line string) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: EnvGuard's file
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// describe says what a turned-away request was: its method and URL, and the envelope's command and
// version, the enums every call home report has (never its payload).
func describe(r *http.Request) string {
	var e struct {
		Command string `json:"command"`
		Version string `json:"version"`
	}
	if r.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, &e)
	}
	return fmt.Sprintf("%s %s: command %q, version %q", r.Method, r.URL.Redacted(), e.Command, e.Version)
}

// hostOf is u's host name, without a port.
func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.Hostname()
}
