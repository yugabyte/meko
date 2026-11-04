package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/tlscert"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// webRetry is how often serve tries again to take the web port while something else holds it (an
// AgentK from before Meko served it), and to hand over from an extension that can't yet.
var webRetry = 30 * time.Second

// webURL is the web listener's address for links: 127.0.0.1 with local access, else [web] name or
// this host's FQDN, over HTTPS.
func webURL(w config.Web) string {
	if w.Access != config.AccessRemote {
		return fmt.Sprintf("http://127.0.0.1:%d", w.Port)
	}
	host := w.Name
	if host == "" {
		host = tlscert.FQDN()
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(w.Port))
}

// webBind is the address the web listener takes: loopback for local access, every address for remote.
func webBind(w config.Web) string {
	if w.Access == config.AccessRemote {
		return net.JoinHostPort("", strconv.Itoa(w.Port))
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(w.Port))
}

// webListener runs the web listener for serve: it hands over from the extensions that serve a web
// UI (handoff), takes the port (waiting while it's held), and serves until ctx ends, while [web]
// enabled is on; Reload has it read its settings again (meko ui start and stop). Status reads its
// state; Shutdown drains it.
type webListener struct {
	l       paths.Layout
	st      *store.Store
	log     *slog.Logger
	reload  chan struct{}                          // Reload's, one waiting at most
	newOpts func(cfg config.Config) server.Options // the web listener's server, for its settings

	mu      sync.Mutex
	status  server.WebStatus
	srv     *server.Server
	sockets map[string]string          // an extension's name to the socket its handoff named
	handed  map[string]bool            // the extensions handed over since this server started
	asked   map[string]time.Time       // when a handoff was last tried for a socket a request needed
	warned  map[string]string          // the last handoff failure logged for an extension, logged once
	proxied map[string]*extproxy.Proxy // one proxy per extension, kept for its WebSockets
	// jobs are the Extensions page's actions, kept across the servers Run builds.
	jobs *server.ExtensionJobs
	// lock is the data folder's update lock, which serve's updater holds too; updater is that
	// updater (nil in tests), which the Extensions page's Check now runs.
	lock    *update.Lock
	updater *update.Updater
	waiting map[string]string // why an extension's automatic update waits
}

// newWebListener is serve's web listener, reported on (Enabled) until Run has read the settings.
func newWebListener(l paths.Layout, st *store.Store, log *slog.Logger) *webListener {
	return &webListener{l: l, st: st, log: log, reload: make(chan struct{}, 1), sockets: map[string]string{},
		handed: map[string]bool{}, asked: map[string]time.Time{}, warned: map[string]string{},
		proxied: map[string]*extproxy.Proxy{}, jobs: server.NewExtensionJobs(), lock: update.NewLock(l.Root),
		waiting: map[string]string{}, status: server.WebStatus{Enabled: true}}
}

// Reload has Run read the settings again, and start or stop the listener as [web] enabled says.
func (w *webListener) Reload() {
	select {
	case w.reload <- struct{}{}:
	default:
	}
}

// socket is where an extension listens. An extension registered since the server started (meko
// extension install) is asked on the first request that needs it, and again at most every 10 s.
func (w *webListener) socket(name string) func() string {
	return func() string {
		w.mu.Lock()
		path, last := w.sockets[name], w.asked[name]
		if path == "" && time.Since(last) > 10*time.Second {
			w.asked[name] = time.Now()
			w.mu.Unlock()
			w.handoff(context.Background())
			w.mu.Lock()
			path = w.sockets[name]
		}
		w.mu.Unlock()
		return path
	}
}

// Status is the web listener's state, for status.
func (w *webListener) Status() *server.WebStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.status
	return &st
}

func (w *webListener) set(f func(*server.WebStatus)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(&w.status)
}

// forget drops where an extension listens, so the next request that needs it asks again (throttled
// by socket): it stopped, or its home moved and its socket with it.
func (w *webListener) forget(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sockets, name)
	delete(w.handed, name)
}

// Run serves until ctx ends. A port that's taken is tried again every webRetry, and so is a handoff
// that failed: once one works, its extension's settings are in web.toml, read before the port is.
// While [web] enabled is off it takes no port, until a Reload finds it on.
func (w *webListener) Run(ctx context.Context) {
	logged := ""
	for ctx.Err() == nil {
		pending := w.handoff(ctx)
		cfg, err := config.Load(w.l.Config, os.Getenv, config.Overrides{})
		if err != nil {
			w.fail(&logged, "can't read the settings", err)
			if !sleep(ctx, webRetry) {
				return
			}
			continue
		}
		w.set(func(s *server.WebStatus) {
			s.URL, s.Access, s.Enabled = webURL(cfg.Web), cfg.Web.Access, cfg.Web.Enabled
			if !s.Enabled {
				s.Error = ""
			}
		})
		if !cfg.Web.Enabled {
			w.log.Info("the web UI is off ([web] enabled = false; meko ui start turns it on)")
			logged = ""
			select {
			case <-ctx.Done():
				return
			case <-w.reload:
			}
			continue
		}
		ln, pair, err := w.listen(cfg)
		if err != nil {
			w.fail(&logged, "the web listener can't start", err)
			select {
			case <-ctx.Done():
				return
			case <-w.reload:
			case <-time.After(webRetry):
			}
			continue
		}
		logged = ""
		srv := server.New(w.newOpts(cfg))
		w.mu.Lock()
		w.srv = srv
		w.status.Listening, w.status.Error = true, ""
		if pair != nil {
			w.status.Fingerprint, w.status.SelfSigned = pair.Fingerprint, pair.SelfSigned
		}
		w.mu.Unlock()
		w.log.Info("web listener started", "url", webURL(cfg.Web), "access", cfg.Web.Access)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(ln) }()
		// An extension that couldn't hand over yet is asked again; when its settings change the
		// listener's, the listener starts again with them.
		iter, stopWatch := context.WithCancel(ctx)
		restart := w.watchHandoffs(iter, pending, cfg)
		select {
		case <-ctx.Done():
		case err = <-served:
			w.log.Error("the web listener stopped", "error", err)
		case <-restart:
			w.log.Info("restarting the web listener with the settings an extension handed over")
		case <-w.reload:
			w.log.Info("reading the web listener's settings again")
		}
		stopWatch()
		w.shutdownServer()
	}
}

func (w *webListener) fail(logged *string, what string, err error) {
	w.set(func(s *server.WebStatus) { s.Listening, s.Error = false, err.Error() })
	if *logged != err.Error() {
		w.log.Warn(what+"; trying again every 30 s", "error", err)
		*logged = err.Error()
	}
}

// listen takes the web port, over TLS with remote access.
func (w *webListener) listen(cfg config.Config) (net.Listener, *tlscert.Pair, error) {
	var pair *tlscert.Pair
	if cfg.Web.Access == config.AccessRemote {
		var err error
		if cfg.TLS.Cert != "" {
			pair, err = tlscert.Load(expandHome(cfg.TLS.Cert), expandHome(cfg.TLS.Key))
		} else {
			pair, err = tlscert.SelfSigned(w.l.Certs, time.Now())
		}
		if err != nil {
			return nil, nil, err
		}
	}
	ln, err := net.Listen("tcp", webBind(cfg.Web))
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, nil, fmt.Errorf("port %d is in use (an AgentK from before Meko served it, or another "+
			"program). Meko takes it once it's free; or pick another with [web] port in config.toml", cfg.Web.Port)
	}
	if err != nil {
		return nil, nil, err
	}
	if pair != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair.Cert}, MinVersion: tls.VersionTLS12})
	}
	return ln, pair, nil
}

// Shutdown drains the web listener.
func (w *webListener) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	srv := w.srv
	w.srv = nil
	w.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// shutdownServer drains the web listener, then closes the WebSockets it proxied, which a shutdown
// leaves open.
func (w *webListener) shutdownServer() {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	_ = w.Shutdown(ctx)
	w.mu.Lock()
	proxies := make([]*extproxy.Proxy, 0, len(w.proxied))
	for _, p := range w.proxied {
		proxies = append(proxies, p)
	}
	w.mu.Unlock()
	for _, p := range proxies {
		p.CloseAll()
	}
	w.set(func(s *server.WebStatus) { s.Listening = false })
}

// Proxies are the registered extensions with a web UI, read from the registry each time, so one
// installed while the server runs is served at once.
func (w *webListener) Proxies() []*extproxy.Proxy {
	reg, err := extensions.Load(w.l.Root)
	if err != nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*extproxy.Proxy
	for _, name := range reg.Names() {
		cat, ok := extensions.Known[name]
		if !ok || !cat.WebUI {
			continue
		}
		if w.proxied[name] == nil {
			p := extproxy.New(name, cat.Title, w.socket(name), w.log)
			p.OnGone = func() { w.forget(name) }
			w.proxied[name] = p
		}
		out = append(out, w.proxied[name])
	}
	return out
}

// notify tells each running extension with a web UI that what changed (extproxy.Notify), one at a
// time; a failure is logged, never the change itself.
func (w *webListener) notify(ctx context.Context, what string) {
	for _, p := range w.Proxies() {
		if err := p.Notify(ctx, what); err != nil {
			w.log.Warn("couldn't tell an extension of a change", "extension", p.Name, "what", what, "error", err)
		}
	}
}

// watchHandoffs asks the extensions that couldn't hand over yet again every webRetry, and closes the
// returned channel when one does and its settings change the listener's.
func (w *webListener) watchHandoffs(ctx context.Context, pending bool, cfg config.Config) <-chan struct{} {
	restart := make(chan struct{})
	if !pending {
		return restart
	}
	go func() {
		for sleep(ctx, webRetry) {
			if w.handoff(ctx) {
				continue
			}
			now, err := config.Load(w.l.Config, os.Getenv, config.Overrides{})
			if err == nil && (now.Web != cfg.Web || now.TLS != cfg.TLS) {
				close(restart)
			}
			return
		}
	}()
	return restart
}

// handoff asks each registered extension with a web UI where it listens, once per server start, and
// takes over the web settings it had when it served browsers itself, filling in only what Meko
// doesn't have yet (so it's safe to repeat): [web] (and [tls] for a certificate its config named)
// and its self-signed certificate. Its owner (login) is ignored: Meko has none. It reports whether
// one is still to ask.
func (w *webListener) handoff(ctx context.Context) (pending bool) {
	reg, err := extensions.Load(w.l.Root)
	if err != nil {
		w.log.Warn("can't read the extensions", "error", err)
		return false
	}
	for _, name := range reg.Names() {
		cat, ok := extensions.Known[name]
		w.mu.Lock()
		done := w.handed[name]
		w.mu.Unlock()
		if !ok || !cat.WebUI || done {
			continue
		}
		h, err := reg.Entries[name].Handoff(w.l.Root)
		if err != nil {
			w.useKnownSocket(ctx, name)
			w.warnOnce(name, "the extension can't hand over yet; asking again every 30 s", err)
			pending = true
			continue
		}
		if err := w.takeOver(ctx, name, h); err != nil {
			w.warnOnce(name, "taking over from the extension failed; asking again every 30 s", err)
			pending = true
			continue
		}
		w.mu.Lock()
		w.sockets[name], w.handed[name] = h.Socket, true
		delete(w.warned, name)
		w.mu.Unlock()
	}
	return pending
}

// warnOnce logs a handoff failure the first time it happens, not at every retry.
func (w *webListener) warnOnce(name, msg string, err error) {
	w.mu.Lock()
	seen := w.warned[name] == err.Error()
	w.warned[name] = err.Error()
	w.mu.Unlock()
	if !seen {
		w.log.Warn(msg, "extension", name, "error", err)
	}
}

// socketKey is where the last socket an extension named is kept, for a start whose handoff fails.
func socketKey(name string) string { return "extension_socket." + name }

// tookOverKey records that an extension's old web settings were taken over, which happens once: the
// extension keeps reporting them, and a [web] the owner removed later must not come back from it.
func tookOverKey(name string) string { return "extension_took_over." + name }

func (w *webListener) useKnownSocket(ctx context.Context, name string) {
	if v, ok, err := w.st.Setting(ctx, socketKey(name)); err == nil && ok {
		w.mu.Lock()
		if w.sockets[name] == "" {
			w.sockets[name] = v
		}
		w.mu.Unlock()
	}
}

func (w *webListener) takeOver(ctx context.Context, name string, h extensions.Handoff) error {
	if err := w.st.SetSetting(ctx, socketKey(name), h.Socket); err != nil {
		return err
	}
	if _, done, err := w.st.Setting(ctx, tookOverKey(name)); err != nil || done {
		return err
	}
	if err := w.importLegacy(name, h.Legacy); err != nil {
		return err
	}
	return w.st.SetSetting(ctx, tookOverKey(name), time.Now().UTC().Format(time.RFC3339))
}

// importLegacy takes over what an extension had when it served browsers itself (nil: nothing).
func (w *webListener) importLegacy(name string, lg *extensions.Legacy) error {
	if lg == nil {
		return nil
	}
	var tlsFiles config.TLS
	if t := lg.TLS; t != nil && t.Cert != "" && t.Key != "" {
		if t.SelfSigned {
			copied, err := copyPair(t.Cert, t.Key, w.l.Certs)
			if err != nil {
				return err
			}
			if copied {
				w.log.Info("took over the extension's self-signed certificate", "extension", name)
			}
		} else {
			tlsFiles = config.TLS{Cert: t.Cert, Key: t.Key}
		}
	}
	if lg.Port != 0 {
		access := config.AccessRemote
		if isLoopback(lg.Bind) {
			access = config.AccessLocal
		}
		added, err := config.AddWeb(w.l.Config, config.Web{Port: lg.Port, Access: access}, tlsFiles)
		if err != nil {
			return err
		}
		if added {
			w.log.Info("took over the extension's web settings", "extension", name, "port", lg.Port, "access", access)
		}
	}
	return nil
}

// copyPair copies a self-signed pair into dir under tlscert's names, unless dir has one already.
func copyPair(certFile, keyFile, dir string) (bool, error) {
	dstCert, dstKey := filepath.Join(dir, tlscert.CertName), filepath.Join(dir, tlscert.KeyName)
	if _, err := os.Stat(dstCert); err == nil {
		return false, nil
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return false, err
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return false, err
	}
	if _, err = tls.X509KeyPair(cert, key); err != nil {
		return false, fmt.Errorf("the certificate in %s doesn't load: %w", certFile, err)
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if err = paths.WriteFile(dstKey, key); err != nil {
		return false, err
	}
	return true, paths.WriteFile(dstCert, cert)
}

func isLoopback(bind string) bool {
	switch bind {
	case "127.0.0.1", "::1", "[::1]", "localhost":
		return true
	}
	return false
}

func expandHome(p string) string {
	if len(p) > 1 && p[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// webAccessNote says which browsers reach the web listener: this computer's, or any that reaches it.
func webAccessNote(w *server.WebStatus) string {
	if w.Access != config.AccessRemote {
		return "local, from this computer"
	}
	return "remote, from every address"
}

// webOffNote is what status and start say while [web] enabled is off.
const webOffNote = "off (meko ui start turns it on)"

// printWeb is status --verbose's lines about the web listener.
func printWeb(stdout io.Writer, w *server.WebStatus) {
	if w == nil {
		return
	}
	if !w.Enabled {
		fmt.Fprintf(stdout, "  Humans:        web UI %s\n", webOffNote)
		return
	}
	fmt.Fprintf(stdout, "  Humans:        %s (web UI), %s\n", w.URL, webAccessNote(w))
	if !w.Listening && w.Error != "" {
		fmt.Fprintf(stdout, "                 not listening: %s\n", w.Error)
	}
	if w.Fingerprint != "" {
		kind := "from [tls]"
		if w.SelfSigned {
			kind = "self-signed"
		}
		fmt.Fprintf(stdout, "  Certificate:   %s, SHA-256 %s\n", kind, w.Fingerprint)
	}
}
