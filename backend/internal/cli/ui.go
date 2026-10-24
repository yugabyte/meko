package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// openBrowser opens a URL in the user's browser; tests replace it.
var openBrowser = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start() //nolint:gosec // G204: the URL is the server's own
}

// runUI opens the web listener's page at path ("/" for Meko's web UI, /ext/agentk/ for AgentK's).
// The link carries no credential: with local access a browser on this computer needs none, and with
// remote access the page signs in. With remote access and no login yet, it prints a setup link instead.
func runUI(l paths.Layout, path string, open bool, stdout, stderr io.Writer) int {
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
	st, err := c.status(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	w := st.Web
	if w == nil || w.URL == "" {
		return fail(stderr, fmt.Errorf("this server has no web listener"))
	}
	if !w.Listening {
		// Another program may hold the port (an AgentK from before Meko served it): don't open its page.
		why := w.Error
		if why == "" {
			why = "it's starting"
		}
		return fail(stderr, fmt.Errorf("the web UI isn't listening at %s yet: %s", w.URL, why))
	}
	if w.Access == config.AccessRemote && !w.Login {
		link, lerr := c.setupLink(ctx)
		if lerr != nil {
			return fail(stderr, lerr)
		}
		fmt.Fprintln(stderr, "No login is set up yet. Open this link to make one; it works once, within 7 days")
		fmt.Fprintln(stdout, link.URL)
		return exitOK
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	link := w.URL + path
	if open {
		if err = openBrowser(link); err != nil {
			fmt.Fprintf(stderr, "meko: couldn't open the browser (%v); open the link below instead\n", err)
		}
	}
	fmt.Fprintln(stdout, link)
	return exitOK
}

// runSetupLink prints a one-time link that sets up the web listener's login, or replaces it (the way
// back in after a lost password: it turns two-step sign-in off and signs every browser out).
func runSetupLink(l paths.Layout, stdout, stderr io.Writer) int {
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
	st, err := c.status(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	link, err := c.setupLink(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, link.URL)
	if st.Web != nil && st.Web.Login {
		fmt.Fprintln(stderr, "The link works once, within 7 days. Using it replaces the login, "+
			"turns two-step sign-in off and signs every browser out")
	} else {
		fmt.Fprintln(stderr, "The link works once, within 7 days")
	}
	if st.Web != nil && st.Web.Access != config.AccessRemote {
		fmt.Fprintln(stderr, "Access is local, so this computer's browser needs no login; the login is for "+
			"remote access ([web] access = \"remote\" in config.toml)")
	}
	return exitOK
}
