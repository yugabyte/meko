package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"time"

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

// runUI opens the running server's web UI with a one-time code in the link's fragment, which the
// page swaps for the client token: there's no login. The link lands on the browser's command
// line, where other local users can read it, so it carries the code (good once, for a minute)
// rather than the token.
func runUI(l paths.Layout, open bool, stdout, stderr io.Writer) int {
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
	code, err := c.uiCode(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	link := fmt.Sprintf("%s/#code=%s", c.base, code.Code)
	if open {
		if err = openBrowser(link); err != nil {
			fmt.Fprintf(stderr, "meko: couldn't open the browser (%v); open the link below instead\n", err)
		}
	}
	fmt.Fprintln(stdout, link)
	if !open {
		fmt.Fprintf(stderr, "The link works once, within %d seconds\n", code.ExpiresIn)
	}
	return exitOK
}
