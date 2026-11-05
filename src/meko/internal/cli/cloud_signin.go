package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/spin"
)

// signInWait is how long meko cloud_link waits for the browser to come back from signing in.
var signInWait = 5 * time.Minute

// terminalIn reports whether meko cloud_link's input is a person at a terminal, who's asked how to
// link; piped input is a key. Tests replace it.
var terminalIn = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && interactiveFile(f)
}

// signInChoices are what chooseSignIn offers, the first the default.
var signInChoices = [...]string{"Sign in with your browser", "Enter an API key"}

// chooseSignIn asks a person at a terminal whether to sign in with the browser (the default) or
// enter an API key, and returns the input to read the rest from. Piped input is a key, unasked. A
// terminal gets a picker (pickKey): ↑ and ↓ move between the choices and Enter picks one, or 1 or 2
// picks it at once. One that can't be switched to reading a key at a time (pickKey's
// errNoKeyMode) is asked for a line, 1 or 2, instead; so is input tests pass as a terminal's.
func chooseSignIn(in io.Reader, prompt io.Writer) (bool, io.Reader, error) {
	if !terminalIn(in) {
		return false, in, nil
	}
	if f, ok := in.(*os.File); ok {
		i, err := pickKey(f, prompt, "How do you want to link Meko Cloud?", signInChoices[:])
		if !errors.Is(err, errNoKeyMode) {
			return i == 0, in, err
		}
	}
	answers := newAnswerReader(in)
	fmt.Fprintf(prompt, "How do you want to link Meko Cloud?\n"+
		"  1) %s\n"+
		"  2) %s\n"+
		"Choose 1 or 2 (Enter for 1): ", signInChoices[0], signInChoices[1])
	line, err := answers.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, nil, err
	}
	switch strings.TrimSpace(line) {
	case "", "1":
		return true, answers, nil
	case "2":
		return false, answers, nil
	}
	return false, nil, errors.New("choose 1 to sign in with your browser, or 2 to enter an API key")
}

// errCanceled is a question left with Ctrl+C or Ctrl+D.
var errCanceled = errors.New("canceled: nothing was changed")

// errNoKeyMode is a terminal pickKey can't read a key at a time: its settings can't be read or set.
var errNoKeyMode = errors.New("the terminal can't be read a key at a time")

// enterWait is how long pickKey waits, after a number picked a choice, for an Enter typed out of
// habit, which it swallows so the next prompt (the API key's) doesn't read it as an empty answer:
// in tenths of a second, as a terminal's VTIME counts.
const enterWait uint8 = 5

// pickKey asks question with choices on the terminal f, read a key at a time with echo off: ↑ and ↓
// (or k and j) move the mark, Enter picks the marked choice, and a choice's number picks it at once.
// Ctrl+C and Ctrl+D cancel. It returns the index picked, and leaves the choices on screen with the
// picked one marked. Before it prints anything, a terminal whose settings can't be read or set is
// errNoKeyMode, for the caller to ask another way.
func pickKey(f *os.File, w io.Writer, question string, choices []string) (int, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errNoKeyMode, err)
	}
	keys := *old
	keys.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG
	keys.Cc[unix.VMIN], keys.Cc[unix.VTIME] = 1, 0
	if err = unix.IoctlSetTermios(fd, ioctlSetTermios, &keys); err != nil {
		return 0, fmt.Errorf("%w: %w", errNoKeyMode, err)
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }()
	color := spin.Color(w)
	draw := func(at int, again bool) {
		if again {
			fmt.Fprintf(w, "\r\033[%dA", len(choices))
		}
		for i, c := range choices {
			line := fmt.Sprintf("  %d) %s", i+1, c)
			switch {
			case i == at && color:
				line = "\033[1m› " + line[2:] + "\033[0m"
			case i == at:
				line = "› " + line[2:]
			}
			fmt.Fprintf(w, "%s\033[K\n", line)
		}
	}
	fmt.Fprintf(w, "%s (↑ and ↓, then Enter, or 1 to %d)\n", question, len(choices))
	at := 0
	draw(at, false)
	buf := make([]byte, 16)
	for {
		n, rerr := f.Read(buf)
		if rerr != nil {
			return 0, rerr
		}
		next, picked, kerr := pickKeyPress(at, len(choices), buf[:n])
		if kerr != nil {
			return 0, kerr
		}
		if next != at {
			at = next
			draw(at, true)
		}
		if picked {
			if !bytes.ContainsAny(buf[:n], "\r\n") { // picked by its number, with no Enter yet
				swallowEnter(f, fd, keys)
			}
			return at, nil
		}
	}
}

// swallowEnter reads, for up to enterWait, what's typed after a number picked a choice, and drops
// it: an Enter typed out of habit, which the next prompt would otherwise take as its answer. keys
// are the terminal's key-at-a-time settings, which it changes to time out.
func swallowEnter(f *os.File, fd int, keys unix.Termios) {
	keys.Cc[unix.VMIN], keys.Cc[unix.VTIME] = 0, enterWait
	if unix.IoctlSetTermios(fd, ioctlSetTermios, &keys) != nil {
		return
	}
	_, _ = f.Read(make([]byte, 16))
}

// pickKeyPress is what one read of keys k does to a picker marking choice at of n: ↑ (an arrow's
// escape sequence, or k) and ↓ (or j) move the mark, wrapping around; Enter picks the marked
// choice; a choice's number marks and picks it; Ctrl+C and Ctrl+D cancel; anything else does nothing.
func pickKeyPress(at, n int, k []byte) (next int, picked bool, err error) {
	arrow := func(c byte) bool { return len(k) >= 3 && k[0] == 0x1b && (k[1] == '[' || k[1] == 'O') && k[2] == c }
	switch {
	case len(k) == 0:
	case arrow('A') || k[0] == 'k':
		return (at + n - 1) % n, false, nil
	case arrow('B') || k[0] == 'j':
		return (at + 1) % n, false, nil
	case k[0] == '\r' || k[0] == '\n':
		return at, true, nil
	case k[0] >= '1' && int(k[0]-'1') < n:
		return int(k[0] - '1'), true, nil
	case k[0] == 0x03 || k[0] == 0x04:
		return at, false, errCanceled
	}
	return at, false, nil
}

// signInWithBrowser signs in to the Meko server at mcpURL as the Meko installer does: it opens the
// browser on the sign-in page (or, with noBrowser, prints its address) and waits for Meko Cloud to
// send the browser back to a callback on 127.0.0.1. A browser on another computer can't reach that
// callback, so the address it ended on may be pasted instead.
func signInWithBrowser(mcpURL string, noBrowser bool, answers io.Reader, out io.Writer) (cloudlink.Minted, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return cloudlink.Minted{}, fmt.Errorf("can't wait for the sign-in: %w", err)
	}
	addr, _ := ln.Addr().(*net.TCPAddr)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)
	ctx, cancel := context.WithTimeout(context.Background(), signInWait+2*cloudlink.SignInRequestTimeout)
	defer cancel()
	flow, err := cloudlink.StartFlow(ctx, nil, mcpURL, redirect, nil)
	if err != nil {
		_ = ln.Close()
		return cloudlink.Minted{}, err
	}

	type back struct {
		code, state string
		err         error
	}
	came := make(chan back, 2)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			code, state, err := cloudlink.Callback(r.URL.String())
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err != nil {
				_, _ = io.WriteString(w, callbackPage("Meko Cloud didn't sign you in",
					"Go back to the terminal to see why."))
			} else {
				_, _ = io.WriteString(w, callbackPage("Signed in to Meko Cloud",
					"You can close this window and go back to the terminal."))
			}
			came <- back{code, state, err}
		})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	switch {
	case noBrowser:
		fmt.Fprintf(out, "Open this address in a browser to sign in to Meko Cloud:\n  %s\n", flow.AuthorizeURL)
	case openBrowser(flow.AuthorizeURL) != nil:
		fmt.Fprintf(out, "Couldn't open a browser. Open this address to sign in to Meko Cloud:\n  %s\n", flow.AuthorizeURL)
	default:
		fmt.Fprintf(out, "Opened your browser to sign in to Meko Cloud. If it didn't open, use this address:\n  %s\n",
			flow.AuthorizeURL)
	}
	fmt.Fprint(out, "\nWaiting for you to sign in. If your browser is on another computer, paste the address "+
		"it ended on here: ")

	// A pasted address is read as well as the callback waited for; the first to come wins. A read that
	// never ends (the callback came first) ends with the command.
	go func() {
		a := newAnswerReader(answers)
		for {
			line, err := a.ReadString('\n')
			if strings.TrimSpace(line) != "" {
				code, state, cerr := cloudlink.Callback(line)
				came <- back{code, state, cerr}
				return
			}
			if err != nil {
				return
			}
		}
	}()

	timer := time.NewTimer(signInWait)
	defer timer.Stop()
	var got back
	select {
	case got = <-came:
	case <-timer.C:
		fmt.Fprintln(out)
		return cloudlink.Minted{}, fmt.Errorf("timed out after %s waiting for the sign-in. Run meko cloud_link "+
			"again, or choose to enter an API key", signInWait)
	}
	fmt.Fprintln(out)
	if got.err != nil {
		return cloudlink.Minted{}, got.err
	}
	return flow.Finish(ctx, got.code, got.state, cloudlink.KeyName(time.Now()))
}

func callbackPage(title, text string) string {
	return "<!doctype html><meta charset=utf-8><title>" + title + "</title>" +
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">` +
		"<h1>" + title + "</h1><p>" + text + "</p></body>"
}
