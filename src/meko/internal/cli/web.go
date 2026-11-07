package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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

// webRetry is how often serve tries again to hand over from an extension that can't yet, and to
// take the port back after it lost it.
var webRetry = 30 * time.Second

// webURL is the web UI's address for links: 127.0.0.1 with local access, else [web] name or this
// host's FQDN, over HTTPS.
func webURL(w config.Web, port int) string {
	if w.Access != config.AccessRemote {
		return fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	host := w.Name
	if host == "" {
		host = tlscert.FQDN()
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// bindHost is the address the port is taken on: every address with remote access while the web UI
// is on (Sniff sorts TLS from plain HTTP), else loopback only, so meko ui stop takes it off the
// network.
func bindHost(w config.Web) string {
	if w.Access == config.AccessRemote && w.Enabled {
		return ""
	}
	return "127.0.0.1"
}

// webListener is serve's one listener, on [server] port, for agents, the CLI and browsers. Bind
// takes the port when serve starts, so a port another program holds stops serve. Run serves, hands
// over from the extensions that serve a web UI (handoff), and takes up their settings and meko ui
// start and stop (Reload): a new server.Server is swapped in under the same socket, which is taken
// again only when the address it's bound to changes (remote access turned on or off). Status reads
// its state; Close drains it.
type webListener struct {
	l       paths.Layout
	st      *store.Store
	log     *slog.Logger
	port    int
	reload  chan struct{}                          // Reload's, one waiting at most
	newOpts func(cfg config.Config) server.Options // the server for the settings, as serve builds it

	cur  atomic.Pointer[server.Server]   // the server requests go to
	cert atomic.Pointer[tls.Certificate] // what TLS serves, with remote access

	mu      sync.Mutex
	status  server.WebStatus
	cfg     config.Config              // the settings taken up
	ln      net.Listener               // the port, from Bind until Run serves it
	host    string                     // the address it's bound to (bindHost, or 127.0.0.1: certErr)
	certErr string                     // why remote access couldn't start: its certificate
	httpSrv *http.Server               // serving the port, while Run does
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

// newWebListener is serve's listener on port, reported on (Enabled) until Bind has read the settings.
func newWebListener(l paths.Layout, st *store.Store, log *slog.Logger, port int) *webListener {
	return &webListener{l: l, st: st, log: log, port: port, reload: make(chan struct{}, 1),
		sockets: map[string]string{}, handed: map[string]bool{}, asked: map[string]time.Time{},
		warned: map[string]string{}, proxied: map[string]*extproxy.Proxy{}, jobs: server.NewExtensionJobs(),
		lock: update.NewLock(l.Root), waiting: map[string]string{}, status: server.WebStatus{Enabled: true}}
}

// Reload has Run read the settings again, and let browsers in or keep them out as [web] enabled says.
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

// Status is the listener's state, for status.
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

// Bind takes the port for cfg, the settings serve started with.
func (w *webListener) Bind(cfg config.Config) error {
	if err := w.bindTo(w.hostFor(cfg, "", false)); err != nil {
		return err
	}
	w.mu.Lock()
	w.cfg = cfg
	w.mu.Unlock()
	w.report(cfg)
	return nil
}

// hostFor is the address to take the port on for cfg (bindHost), reading the certificate when
// that's every address. A certificate that can't be read keeps the port on 127.0.0.1, so agents
// and the CLI keep it, and status says why browsers elsewhere can't reach Meko (certErr) until a
// later change reads one; except while TLS already serves one (bound to every address, up), which
// is kept.
func (w *webListener) hostFor(cfg config.Config, bound string, up bool) string {
	host := bindHost(cfg.Web)
	if host != "" {
		w.setCertErr("")
		return host
	}
	err := w.loadCert(cfg)
	switch {
	case err == nil:
		w.setCertErr("")
	case up && bound == "":
		w.log.Warn("can't read the certificate; keeping the one it has", "error", err)
	default:
		w.log.Error("can't read the certificate, so remote access is off: Meko listens on 127.0.0.1 only",
			"error", err)
		w.setCertErr("remote access is off, since the certificate can't be read: " + err.Error())
		host = "127.0.0.1"
	}
	return host
}

func (w *webListener) setCertErr(why string) {
	w.mu.Lock()
	w.certErr = why
	w.mu.Unlock()
}

// open takes the port on host: over Sniff, with the certificate, when it's every address ("").
func (w *webListener) open(host string) (net.Listener, error) {
	ln, err := server.ListenOn(host, w.port)
	if err != nil {
		return nil, err
	}
	if host == "" {
		ln = server.Sniff(ln, &tls.Config{MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return w.cert.Load(), nil }})
	}
	return ln, nil
}

// loadCert reads the certificate remote access serves: [tls]'s, or the self-signed pair, made now
// or again within 30 days of expiring. TLS takes the new one at once.
func (w *webListener) loadCert(cfg config.Config) error {
	var pair *tlscert.Pair
	var err error
	if cfg.TLS.Cert != "" {
		pair, err = tlscert.Load(expandHome(cfg.TLS.Cert), expandHome(cfg.TLS.Key))
	} else {
		pair, err = tlscert.SelfSigned(w.l.Certs, time.Now())
	}
	if err != nil {
		return err
	}
	w.cert.Store(&pair.Cert)
	w.set(func(s *server.WebStatus) { s.Fingerprint, s.SelfSigned = pair.Fingerprint, pair.SelfSigned })
	return nil
}

// report is what status says once the listener has taken up cfg: listening, and why browsers
// elsewhere can't reach it when remote access couldn't start.
func (w *webListener) report(cfg config.Config) {
	w.set(func(s *server.WebStatus) {
		s.URL, s.Access, s.Enabled, s.Listening, s.Error = webURL(cfg.Web, w.port), cfg.Web.Access, cfg.Web.Enabled, true,
			w.certErr
		if w.host != "" {
			s.Fingerprint, s.SelfSigned = "", false
		}
	})
}

// Run serves the port Bind took until ctx ends, then drains it. It hands over from the extensions
// that serve a web UI first, and again every webRetry while one can't yet; it takes up their
// settings and a Reload's as they come.
func (w *webListener) Run(ctx context.Context) {
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()
	w.cur.Store(server.New(w.newOpts(cfg)))
	served := w.serve()
	for {
		pending := w.handoff(ctx)
		if next, err := config.Load(w.l.Config, os.Getenv, config.Overrides{}); err != nil {
			w.log.Warn("can't read the settings again; keeping the ones it has", "error", err)
		} else {
			cfg, served = w.takeUp(cfg, next, served)
		}
		iter, stopWatch := context.WithCancel(ctx)
		restart := w.watchHandoffs(iter, pending, cfg)
		var retry <-chan time.Time
		if served == nil {
			retry = time.After(webRetry)
		}
		select {
		case <-ctx.Done():
			stopWatch()
			w.stop()
			return
		case err := <-served:
			w.log.Error("the listener stopped; taking the port again every 30 s", "error", err)
			w.set(func(s *server.WebStatus) { s.Listening, s.Error = false, err.Error() })
			w.stop()
			served = nil
		case <-retry:
		case <-restart:
			w.log.Info("taking up the settings an extension handed over")
		case <-w.reload:
			w.log.Info("reading the web UI's settings again")
		}
		stopWatch()
	}
}

// serve starts serving the bound port, and returns what its Serve returns, or nil with no port.
func (w *webListener) serve() <-chan error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ln == nil {
		return nil
	}
	hs := &http.Server{ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.cur.Load().ServeHTTP(rw, r) })}
	ln := w.ln
	w.httpSrv, w.ln = hs, nil
	served := make(chan error, 1)
	go func() {
		if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			served <- err
		}
	}()
	return served
}

// takeUp moves from the settings now taken up to next: the port is taken again when the address
// it's bound to changes (or it was lost), and else only the server is swapped. When the new address
// can't be taken, the old one is taken back, and the old settings kept. It returns the settings
// taken up and what's serving (nil: nothing, to try again).
func (w *webListener) takeUp(now, next config.Config, served <-chan error) (config.Config, <-chan error) {
	w.mu.Lock()
	host, up := w.host, w.httpSrv != nil
	w.mu.Unlock()
	want := w.hostFor(next, host, up)
	rebind := !up || want != host
	if rebind {
		w.stop()
		err := w.bindTo(want)
		if err != nil && up {
			w.log.Error("can't take the port for the new settings; keeping the old ones", "error", err)
			if err = w.bindTo(host); err == nil {
				next = now
			}
		}
		if err != nil {
			w.log.Error("can't take the port; trying again every 30 s", "error", err)
			w.set(func(s *server.WebStatus) { s.Listening, s.Error = false, err.Error() })
			return now, nil
		}
	}
	w.cur.Store(server.New(w.newOpts(next)))
	if now.Web.Enabled && !next.Web.Enabled {
		w.closeWebSockets()
	}
	if next.Web != now.Web || rebind {
		w.log.Info("listening", "url", webURL(next.Web, w.port), "access", next.Web.Access,
			"web_ui", map[bool]string{true: "on", false: "off"}[next.Web.Enabled])
	}
	w.mu.Lock()
	w.cfg = next
	w.mu.Unlock()
	w.report(next)
	if rebind {
		served = w.serve()
	}
	return next, served
}

// bindTo takes the port on host, after stop.
func (w *webListener) bindTo(host string) error {
	ln, err := w.open(host)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.ln, w.host = ln, host
	w.mu.Unlock()
	return nil
}

// Close drains the port, or closes it when Run never served it.
func (w *webListener) Close() {
	w.mu.Lock()
	ln := w.ln
	w.ln = nil
	w.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	w.stop()
}

// stop drains the port (drainTimeout), then closes the WebSockets the extensions' proxies upgraded,
// which a drain leaves open.
func (w *webListener) stop() {
	w.mu.Lock()
	hs := w.httpSrv
	w.httpSrv = nil
	w.mu.Unlock()
	if hs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		w.log.Warn("requests didn't finish in time", "error", err)
	}
	w.closeWebSockets()
	w.set(func(s *server.WebStatus) { s.Listening = false })
}

// closeWebSockets closes the WebSockets the extensions' proxies upgraded: AgentK's terminals.
func (w *webListener) closeWebSockets() {
	w.mu.Lock()
	proxies := make([]*extproxy.Proxy, 0, len(w.proxied))
	for _, p := range w.proxied {
		proxies = append(proxies, p)
	}
	w.mu.Unlock()
	for _, p := range proxies {
		p.CloseAll()
	}
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
	// A port says it served browsers itself. Its access is taken over, never its port: Meko's is
	// [server] port, where agents connect too.
	if lg.Port != 0 {
		access := config.AccessRemote
		if isLoopback(lg.Bind) {
			access = config.AccessLocal
		}
		added, err := config.AddWeb(w.l.Config, config.Web{Access: access}, tlsFiles)
		if err != nil {
			return err
		}
		if added {
			w.log.Info("took over the extension's web settings", "extension", name, "access", access)
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

// webAccessNote says which browsers reach the web UI: this computer's, or any that reaches it.
func webAccessNote(w *server.WebStatus) string {
	if w.Access != config.AccessRemote {
		return "local, from this computer"
	}
	return "remote, from every address"
}

// webOffNote is what status and start say while [web] enabled is off.
const webOffNote = "off (meko ui start turns it on)"

// printWeb is status --verbose's lines about the web UI.
func printWeb(stdout io.Writer, w *server.WebStatus) {
	if w == nil {
		return
	}
	if !w.Enabled {
		fmt.Fprintf(stdout, "  Humans:        web UI %s\n", webOffNote)
		return
	}
	fmt.Fprintf(stdout, "  Humans:        %s (web UI), %s\n", w.URL, webAccessNote(w))
	switch {
	case !w.Listening && w.Error != "":
		fmt.Fprintf(stdout, "                 not listening: %s\n", w.Error)
	case w.Error != "":
		fmt.Fprintf(stdout, "                 %s\n", w.Error)
	}
	if w.Fingerprint != "" {
		kind := "from [tls]"
		if w.SelfSigned {
			kind = "self-signed"
		}
		fmt.Fprintf(stdout, "  Certificate:   %s, SHA-256 %s\n", kind, w.Fingerprint)
	}
}
