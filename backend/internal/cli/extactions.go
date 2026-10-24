package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
)

// Installed names the registered extensions, for the web UI's Extensions page.
func (w *webListener) Installed() []string {
	reg, err := extensions.Load(w.l.Root)
	if err != nil {
		return nil
	}
	return reg.Names()
}

// RunExtension runs an action of the Extensions page on a known extension (server.Web.Run): install
// it as meko install_extension does, with no terminal, or run its own start or stop. The output and
// the error reach a browser, so the client token the installer is given is blanked in both. After
// an install or a start, the next request asks the extension where it listens at once.
func (w *webListener) RunExtension(ctx context.Context, name, action string, out io.Writer) error {
	var tok string
	if cfg, err := config.Load(w.l.Config, os.Getenv, config.Overrides{}); err == nil {
		tok, _ = readClientToken(w.l, cfg)
	}
	r := &redactor{w: out, secret: []byte(tok)}
	err := w.runExtension(ctx, name, action, r)
	r.Flush()
	if err != nil {
		if tok != "" {
			return errors.New(strings.ReplaceAll(err.Error(), tok, redacted))
		}
		return err
	}
	if action != "stop" {
		w.reconnect(name)
	}
	return nil
}

func (w *webListener) runExtension(ctx context.Context, name, action string, out io.Writer) error {
	switch action {
	case "install":
		inst, cancel := context.WithTimeout(ctx, extensions.InstallTimeout)
		defer cancel()
		return installExtension(inst, w.l, name, false, nil, out, out)
	case "start", "stop":
		reg, err := extensions.Load(w.l.Root)
		if err != nil {
			return err
		}
		e, ok := reg.Entries[name]
		if !ok {
			return fmt.Errorf("%s isn't installed", name)
		}
		if _, err := os.Stat(e.Command); errors.Is(err, os.ErrNotExist) {
			if findAgain(w.l, reg, name) == "" {
				return fmt.Errorf("%s is gone (%s); meko uninstall_extension %s forgets it", name, e.Command, name)
			}
			e = reg.Entries[name]
		}
		return e.Run(w.l.Root, action, out, out)
	}
	return fmt.Errorf("no action %q", action)
}

// reconnect forgets where an extension listens and when it was last asked, so the next request
// that needs it asks it again at once rather than up to 10 s later.
func (w *webListener) reconnect(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sockets, name)
	delete(w.handed, name)
	delete(w.asked, name)
}

// redacted stands in for a secret in output.
const redacted = "[hidden]"

// redactor passes output on with a secret blanked, holding back only an end that could be the start
// of the secret until more comes or Flush. exec calls Write from one goroutine at a time when
// Stdout and Stderr are the same writer.
type redactor struct {
	w      io.Writer
	secret []byte
	buf    []byte
}

func (r *redactor) Write(p []byte) (int, error) {
	if len(r.secret) == 0 {
		return r.w.Write(p)
	}
	r.buf = bytes.ReplaceAll(append(r.buf, p...), r.secret, []byte(redacted))
	keep := partialPrefix(r.buf, r.secret)
	if _, err := r.w.Write(r.buf[:len(r.buf)-keep]); err != nil {
		return 0, err
	}
	r.buf = append(r.buf[:0], r.buf[len(r.buf)-keep:]...)
	return len(p), nil
}

// Flush writes what was held back: the output ended without the rest of the secret.
func (r *redactor) Flush() {
	if len(r.buf) > 0 {
		_, _ = r.w.Write(r.buf)
		r.buf = r.buf[:0]
	}
}

// partialPrefix is the length of the longest end of b that's a start of secret (shorter than it).
func partialPrefix(b, secret []byte) int {
	for n := min(len(b), len(secret)-1); n > 0; n-- {
		if bytes.HasSuffix(b, secret[:n]) {
			return n
		}
	}
	return 0
}
