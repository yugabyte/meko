package cli

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"github.com/yugabyte/meko/backend/internal/auth"
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

// runUI opens the running server's web UI with the client token in the link's fragment, which the
// page keeps and the browser never sends: there's no login, and only someone who can read the data
// folder gets in.
func runUI(l paths.Layout, open bool, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		fmt.Fprintln(stderr, "meko: Meko isn't running. Start it with meko start.")
		return exitNotRunning
	}
	port, err := readInt(l.Port)
	if err != nil {
		return fail(stderr, err)
	}
	tok, err := auth.ReadClientToken(l.ClientToken)
	if err != nil {
		return fail(stderr, err)
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/#token=%s", port, tok)
	if open {
		if err = openBrowser(link); err != nil {
			fmt.Fprintf(stderr, "meko: couldn't open the browser (%v); open the link below instead.\n", err)
		}
	}
	fmt.Fprintln(stdout, link)
	return exitOK
}
