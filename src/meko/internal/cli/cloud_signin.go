package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
)

// signInWait is how long meko cloud_link waits for the browser to come back from signing in.
var signInWait = 5 * time.Minute

// terminalIn reports whether meko cloud_link's input is a person at a terminal, who's asked how to
// link; piped input is a key. Tests replace it.
var terminalIn = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && interactiveFile(f)
}

// chooseSignIn asks a person at a terminal whether to sign in with the browser (the default) or
// enter an API key, and returns the input to read the rest from. Piped input is a key, unasked.
func chooseSignIn(in io.Reader, prompt io.Writer) (bool, io.Reader, error) {
	if !terminalIn(in) {
		return false, in, nil
	}
	answers := newAnswerReader(in)
	fmt.Fprint(prompt, "How do you want to link Meko Cloud?\n"+
		"  1) Sign in with your browser\n"+
		"  2) Enter an API key\n"+
		"Choose 1 or 2 (Enter for 1): ")
	line, err := answers.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, nil, err
	}
	rest := io.Reader(answers)
	if _, file := in.(*os.File); file {
		rest = in // a terminal hands over a line at a time, so nothing is left in answers; readSecret turns echo off on it
	}
	switch strings.TrimSpace(line) {
	case "", "1":
		return true, answers, nil
	case "2":
		return false, rest, nil
	}
	return false, nil, errors.New("choose 1 to sign in with your browser, or 2 to enter an API key")
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
	fmt.Fprint(out, "Waiting for you to sign in. If your browser is on another computer, paste the address "+
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
