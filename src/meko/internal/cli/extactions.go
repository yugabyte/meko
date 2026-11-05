package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// ExtensionUpdates is what the Extensions page (and AgentK's page) shows about an installed
// extension's updates (extensionUpdates), with why an automatic update waits.
func (w *webListener) ExtensionUpdates(name string) server.ExtensionUpdate {
	u := server.ExtensionUpdate{RolledBack: []string{}}
	if _, ok := extensions.Known[name]; !ok {
		return u
	}
	reg, err := extensions.Load(w.l.Root)
	if err != nil {
		return u
	}
	e, ok := reg.Entries[name]
	if !ok {
		return u
	}
	u = extensionUpdates(w.l.Root, name, e, w.autoUpdateOf(name, e))
	w.mu.Lock()
	u.Waiting = w.waiting[name]
	w.mu.Unlock()
	return u
}

// extensionUpdates is what the Extensions page and meko extension list say about an installed known
// extension's updates (e, its registry entry): the release Meko installed, the newest the last check
// found, the update on probation, the versions rolled back, and Update automatically (auto).
func extensionUpdates(root, name string, e extensions.Entry, auto bool) server.ExtensionUpdate {
	u := server.ExtensionUpdate{RolledBack: []string{}, AutoUpdate: auto}
	cat := extensions.Known[name]
	u.Version = cat.Release(root, name, e.Command)
	u.Updatable = u.Version != ""
	if !u.Updatable {
		if home, err := os.UserHomeDir(); err == nil && cat.Managed {
			u.NotUpdatable = cat.WhyNotUpdatable(root, home, name, e.Command)
		}
		return u
	}
	app := cat.App(root, name)
	bad := app.BadVersions()
	if st, ok := update.ReadState(root); ok {
		checked := st.CheckedAt
		u.CheckedAt, u.CheckError = &checked, st.Error
		es := st.Extensions[name]
		u.Latest, u.LatestURL = es.Latest, es.LatestURL
		// update.json can predate a rollback, and still name the version rolled back.
		u.UpdateAvailable = !bad[es.Latest] && extensions.Newer(u.Version, es.Latest)
	}
	u.Pending, _ = app.ReadPending()
	for v := range bad {
		u.RolledBack = append(u.RolledBack, v)
	}
	sort.Strings(u.RolledBack)
	return u
}

// watchUpdates makes u's checks the extensions' too: they find the extensions' releases and install
// the updates set to install on their own, and u holds the lock the Extensions page's updates take.
func (w *webListener) watchUpdates(u *update.Updater) {
	u.Lock = w.lock
	u.Extensions = func(rels []update.Release) map[string]update.ExtensionState {
		return extensions.Check(w.l.Root, rels)
	}
	u.AfterCheck = w.autoUpdate
	w.updater = u
}

// CheckExtensions checks for releases now (Meko's and the extensions', one list), for the
// Extensions page's Check now. It fails with update.ErrBusy while an update holds the lock; a check
// that fails otherwise isn't an error here, the rows say why.
func (w *webListener) CheckExtensions(ctx context.Context) error {
	if w.updater == nil {
		return errors.New("this server doesn't check for updates")
	}
	_, err := w.updater.CheckNow(ctx)
	return err
}

// SetAutoUpdate is the Extensions page's Update automatically.
func (w *webListener) SetAutoUpdate(name string, on bool) error { return setAutoUpdate(w.l, name, on) }

// autoUpdate installs, after a scheduled check and with the update lock held, the updates of the
// extensions set to Update automatically, each as its Extensions-page job (marked auto). One on
// probation, or that says it's busy (a talk is live), waits for the next check.
func (w *webListener) autoUpdate(ctx context.Context) {
	st, ok := update.ReadState(w.l.Root)
	if !ok {
		return
	}
	reg, err := extensions.Load(w.l.Root)
	if err != nil {
		return
	}
	for _, name := range reg.Names() {
		cat, known := extensions.Known[name]
		e := reg.Entries[name]
		cur := cat.Release(w.l.Root, name, e.Command)
		latest := st.Extensions[name].Latest
		if !known || !w.autoUpdateOf(name, e) || cur == "" || !extensions.Newer(cur, latest) ||
			cat.App(w.l.Root, name).BadVersions()[latest] {
			w.setWaiting(name, "")
			continue
		}
		if p, _ := cat.App(w.l.Root, name).ReadPending(); p != nil {
			w.setWaiting(name, "its last update is still on probation")
			continue
		}
		if cat.Busy(w.l.Root, e) {
			w.setWaiting(name, "in use")
			w.log.Info("an extension's update waits: it's in use", "extension", name, "version", latest)
			continue
		}
		w.setWaiting(name, "")
		w.log.Info("updating an extension on its own", "extension", name, "from", cur, "to", latest)
		started, err := w.jobs.RunSync(name, "update", true, func(jctx context.Context, out io.Writer) error {
			return w.updateExtension(jctx, name, out)
		})
		switch {
		case !started:
			w.log.Info("an extension's update waits: another of its actions is running", "extension", name)
		case err != nil:
			w.log.Warn("an extension's update failed", "extension", name, "error", err)
		default:
			w.log.Info("an extension updated on its own", "extension", name, "version", latest)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// autoUpdateOf is an installed extension's Update automatically (Catalog.AutoUpdate). An unset
// switch that its own old updater's setting turns off is saved off, so it stays off when that
// setting goes.
func (w *webListener) autoUpdateOf(name string, e extensions.Entry) bool {
	on := extensions.Known[name].AutoUpdate(w.l.Root, name, e)
	if e.AutoUpdate == nil && !on {
		if err := setAutoUpdate(w.l, name, false); err != nil {
			w.log.Warn("can't save an extension's Update automatically", "extension", name, "error", err)
		}
	}
	return on
}

func (w *webListener) setWaiting(name, why string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if why == "" {
		delete(w.waiting, name)
	} else {
		w.waiting[name] = why
	}
}

// updateExtension updates an extension, as meko extension update does, for a caller that holds the
// update lock, and waits for one with a web UI to answer when it runs afterwards.
func (w *webListener) updateExtension(ctx context.Context, name string, out io.Writer) error {
	cfg, err := config.Load(w.l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	upd, cancel := context.WithTimeout(ctx, extensions.InstallTimeout)
	var notices unsignedNotices
	runs, err := extensions.Update(upd, extensions.UpdateOptions{Root: w.l.Root, Home: home,
		Client: releaseClient(cfg.Update, func(s string) { notices.add(s); unsignedLog(w.log)(s) }), Out: out}, name)
	cancel()
	notices.print(out)
	w.reconnect(name) // a restarted server's socket is asked for again
	if err != nil || !runs || !extensions.Known[name].WebUI {
		return err
	}
	return w.awaitListening(ctx, name)
}

// Installed names the registered extensions, for the web UI's Extensions page.
func (w *webListener) Installed() []string {
	reg, err := extensions.Load(w.l.Root)
	if err != nil {
		return nil
	}
	return reg.Names()
}

// RunExtension runs an action of the Extensions page on a known extension (server.Web.Run): install
// it as meko extension install does, with no terminal, update it as meko extension update does
// (holding the update lock), or run its own start or stop. The output and the error reach a
// browser, so the client token the installer is given is blanked in both. After an install or a
// start of one with a web UI, or an update that left it running, it waits for the extension to
// answer on its socket (AgentK's start returns before its server listens), so the page's next look
// sees it running.
func (w *webListener) RunExtension(ctx context.Context, name, action string, out io.Writer) error {
	var tok string
	if cfg, err := config.Load(w.l.Config, os.Getenv, config.Overrides{}); err == nil {
		tok, _ = readClientToken(w.l, cfg)
	}
	r := &redactor{w: out, secret: []byte(tok)}
	var err error
	if action == "update" {
		if !w.lock.TryLock() {
			err = update.ErrLocked
		} else {
			err = w.updateExtension(ctx, name, r)
			w.lock.Unlock()
		}
	} else {
		err = w.runExtension(ctx, name, action, r)
	}
	r.Flush()
	if err != nil {
		if tok != "" {
			return errors.New(strings.ReplaceAll(err.Error(), tok, redacted))
		}
		return err
	}
	if (action == "install" || action == "start") && extensions.Known[name].WebUI {
		return w.awaitListening(ctx, name)
	}
	return nil
}

// awaitListening waits up to extensions.ListenTimeout for an extension to answer on the socket its
// handoff names, asking again each second: a look at the list while it starts forgets a socket
// that doesn't answer yet and holds off asking for 10 s (reconnect undoes that). Once it answers,
// the socket is known, so the next request goes straight to it.
func (w *webListener) awaitListening(ctx context.Context, name string) error {
	cat := extensions.Known[name]
	deadline := time.Now().Add(extensions.ListenTimeout)
	for {
		w.reconnect(name)
		if path := w.socket(name)(); path != "" && extproxy.Answers(path) {
			return nil
		}
		if time.Now().After(deadline) {
			w.reconnect(name)
			return fmt.Errorf("%s started, but isn't answering after %s: see %s status", cat.Title,
				extensions.ListenTimeout, cat.Command)
		}
		select {
		case <-ctx.Done():
			w.reconnect(name)
			return errors.New("it was stopped: Meko is stopping")
		case <-time.After(time.Second):
		}
	}
}

func (w *webListener) runExtension(ctx context.Context, name, action string, out io.Writer) error {
	switch action {
	case "install":
		inst, cancel := context.WithTimeout(ctx, extensions.InstallTimeout)
		defer cancel()
		return installExtension(inst, w.l, name, extInstallOptions{}, false, nil, out, out)
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
				return fmt.Errorf("%s %s", extensionTitle(name), extensions.Damaged(name, e.Command))
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
