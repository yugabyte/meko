package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
)

// canOpenBrowser reports whether there's a browser here to open a link in: none over SSH, or on
// Linux without a display. Tests replace it.
var canOpenBrowser = func() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	return runtime.GOOS == "darwin" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// openBrowser opens a URL in the user's browser; tests replace it.
var openBrowser = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start() //nolint:gosec // G204: the URL is the server's own
}

// runUI opens Meko's web UI in the browser, when there's one here, and prints its link. The link
// carries no credential: there's no login, with local or remote access.
func runUI(l paths.Layout, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		fmt.Fprintln(stderr, "meko: Meko isn't running. Start it with meko start")
		return exitNotRunning
	}
	c, err := newClient(l)
	if err != nil {
		return fail(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The port may be being taken again (remote access turned on or off): wait for it, but not for
	// one that says why it can't listen.
	st, err := waitWebListening(ctx, c)
	if err != nil {
		return fail(stderr, err)
	}
	w := st.Web
	if w == nil || w.URL == "" {
		return fail(stderr, errors.New("this server has no web UI"))
	}
	if !w.Enabled {
		return fail(stderr, errWebOff)
	}
	if !w.Listening {
		// Another program may hold the port (a Kode from before Meko served it): don't open its page.
		why := w.Error
		if why == "" {
			why = "it's starting"
		}
		return fail(stderr, fmt.Errorf("the web UI isn't listening at %s yet: %s", w.URL, why))
	}
	if w.Error != "" {
		return fail(stderr, fmt.Errorf("the web UI can't be reached at %s: %s", w.URL, w.Error))
	}
	link := w.URL + "/"
	if canOpenBrowser() {
		if err = openBrowser(link); err != nil {
			fmt.Fprintf(stderr, "meko: couldn't open the browser (%v); open the link below instead\n", err)
		}
	}
	fmt.Fprintln(stdout, link)
	return exitOK
}

var errWebOff = errors.New("the web UI is off. Turn it on with meko ui start")

// runUISwitch is meko ui start (on) and meko ui stop: it sets [web] enabled in web.toml, and has a
// running server let browsers in or keep them out now (agents and the CLI keep the port); a stopped
// one reads it when it starts.
func runUISwitch(l paths.Layout, on bool, stdout, stderr io.Writer) int {
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return fail(stderr, err)
	}
	state := map[bool]string{true: "on", false: "off"}[on]
	if cfg.Web.Enabled == on {
		fmt.Fprintf(stdout, "The web UI is already %s\n", state)
		return exitOK
	}
	if err = config.SetWebEnabled(l.Config, on); errors.Is(err, config.ErrWebEnabledSet) {
		return fail(stderr, errors.New("config.toml sets [web] enabled; change it there and run meko restart"))
	} else if err != nil {
		return fail(stderr, err)
	}
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		if on {
			fmt.Fprintln(stdout, "Turned the web UI on; it starts with Meko (meko start)")
		} else {
			fmt.Fprintln(stdout, "Turned the web UI off; it stays off when Meko starts")
		}
		return exitOK
	}
	c, err := newClient(l)
	if err != nil {
		return fail(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), webWait+drainTimeout)
	defer cancel()
	if err = c.reloadWeb(ctx); err != nil {
		return fail(stderr, err)
	}
	st, err := waitWebSwitched(ctx, c, on)
	switch {
	case err != nil:
		return fail(stderr, err)
	case st.Enabled != on:
		return fail(stderr, fmt.Errorf("the web UI is %s in the settings, but Meko hasn't taken it up yet "+
			"(meko status says when it has)", state))
	case st.Listening && st.Error != "":
		return fail(stderr, fmt.Errorf("the web UI is %s, but can't be reached at %s: %s", state, st.URL, st.Error))
	case !st.Listening:
		why := st.Error
		if why == "" {
			why = "it's taking the port again"
		}
		return fail(stderr, fmt.Errorf("the web UI is %s, but Meko isn't listening at %s yet: %s", state, st.URL, why))
	case on:
		fmt.Fprintf(stdout, "Started the web UI at %s\n", st.URL)
	default:
		fmt.Fprintln(stdout, "Stopped the web UI")
	}
	return exitOK
}

// waitWebSwitched waits, until ctx ends, for the server to take up [web] enabled = on and listen,
// or say why it can't. With remote access the port is taken again (every address on, 127.0.0.1
// off), so a status read that finds it closed for a moment is read again.
func waitWebSwitched(ctx context.Context, c *client, on bool) (server.WebStatus, error) {
	var w server.WebStatus
	for {
		st, err := c.status(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return w, nil
		case err == nil && st.Web == nil:
			return w, errors.New("this server has no web UI")
		case err == nil:
			w = *st.Web
			if w.Enabled == on && (w.Listening || w.Error != "") {
				return w, nil
			}
		}
		select {
		case <-ctx.Done():
			return w, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
