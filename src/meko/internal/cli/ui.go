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
	// The web listener binds just after the API listener answers (after the extensions' handoff), so
	// right after meko start it may still be starting: wait for it, but not for one that says why it
	// can't listen (another program on its port).
	st, err := waitWebListening(ctx, c)
	if err != nil {
		return fail(stderr, err)
	}
	w := st.Web
	if w == nil || w.URL == "" {
		return fail(stderr, fmt.Errorf("this server has no web listener"))
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
// running server start or stop the web listener now; a stopped one reads it when it starts.
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
	case on && !st.Listening:
		why := st.Error
		if why == "" {
			why = "it's still starting (meko status says when it's up)"
		}
		return fail(stderr, fmt.Errorf("the web UI is on, but isn't listening at %s yet: %s", st.URL, why))
	case on:
		fmt.Fprintf(stdout, "Started the web UI at %s\n", st.URL)
	case st.Listening:
		return fail(stderr, errors.New("the web UI is off, but is still stopping (meko status says when it has)"))
	default:
		fmt.Fprintln(stdout, "Stopped the web UI")
	}
	return exitOK
}

// waitWebSwitched waits, until ctx ends, for the web listener to take up [web] enabled = on: to
// listen, or say why it can't, when on; to stop listening when off.
func waitWebSwitched(ctx context.Context, c *client, on bool) (server.WebStatus, error) {
	for {
		st, err := c.status(ctx)
		if err != nil {
			return server.WebStatus{}, err
		}
		if st.Web == nil {
			return server.WebStatus{}, errors.New("this server has no web listener")
		}
		w := *st.Web
		if w.Enabled == on && (on && (w.Listening || w.Error != "") || !on && !w.Listening) {
			return w, nil
		}
		select {
		case <-ctx.Done():
			return w, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
