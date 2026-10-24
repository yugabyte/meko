// Package extproxy serves an extension's web UI on the web listener at /ext/<name>/: it passes each
// request to the unix socket the extension listens on, with the prefix stripped, and keeps the
// WebSockets it upgrades by the browser session that opened them, so a sign-out closes them.
//
// The extension trusts every request on its socket (only the user who runs Meko can connect to it),
// so what reaches it is only what the web listener let in. The browser's Host and Origin are passed
// as they are, so the extension's own same-origin checks still hold; its Cookie header (Meko's
// session) is dropped, and no X-Forwarded-* headers are added.
package extproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Proxy serves one extension.
type Proxy struct {
	Name   string // its catalog name, as in /ext/<name>/
	Title  string // what people call it
	prefix string // "/ext/<name>"
	socket func() string
	// OnGone, if set, hears that the socket it was given is gone or refuses connections (the
	// extension stopped, or its home moved), so the next request asks where it listens again.
	OnGone func()
	log    *slog.Logger
	rp     *httputil.ReverseProxy

	mu    sync.Mutex
	conns map[string]map[net.Conn]struct{} // a session's hash to its upgraded connections
}

// New returns the proxy for an extension whose socket's path socket reports ("" while unknown).
func New(name, title string, socket func() string, log *slog.Logger) *Proxy {
	p := &Proxy{Name: name, Title: title, prefix: "/ext/" + name, socket: socket, log: log,
		conns: map[string]map[net.Conn]struct{}{}}
	p.rp = &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				path := p.socket()
				if path == "" {
					return nil, errNoSocket
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			MaxIdleConns: 16, IdleConnTimeout: 90 * time.Second,
		},
		// Streams (a log, a long poll) reach the browser as they're written.
		FlushInterval: -1,
		ErrorHandler:  p.unavailable,
	}
	return p
}

var errNoSocket = errors.New("the extension hasn't said where it listens")

// Path is where browsers find it: /ext/<name>/.
func (p *Proxy) Path() string { return p.prefix + "/" }

// Available reports whether the extension answers on its socket: a socket file an extension left
// when it died refuses the connection, and is forgotten as a gone one is.
func (p *Proxy) Available() bool {
	path := p.socket()
	if path == "" {
		return false
	}
	if !Answers(path) {
		p.gone()
		return false
	}
	return true
}

// Answers reports whether something accepts connections on the unix socket at path.
func Answers(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (p *Proxy) gone() {
	if p.OnGone != nil {
		p.OnGone()
	}
}

// rewrite strips the prefix and keeps the browser's Host and Origin. ReverseProxy has already
// removed hop-by-hop headers and any X-Forwarded-* the browser sent, and adds none in this mode.
func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	r.Out.URL.Scheme, r.Out.URL.Host = "http", p.Name
	r.Out.URL.Path = strip(r.In.URL.Path, p.prefix)
	if r.In.URL.RawPath != "" {
		r.Out.URL.RawPath = strip(r.In.URL.RawPath, p.prefix)
	}
	r.Out.Host = r.In.Host
	for _, h := range []string{"Cookie", "Authorization", "Forwarded", "X-Real-Ip"} {
		r.Out.Header.Del(h)
	}
}

func strip(path, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || rest[0] != '/' {
		return "/" + rest
	}
	return rest
}

// unavailable answers when the extension can't be reached: a short page for a browser, the API's
// envelope for a script.
func (p *Proxy) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	p.log.Warn("extension unavailable", "extension", p.Name, "error", err)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		p.gone()
	}
	msg := fmt.Sprintf("%s isn't answering. Start it with meko start (or %s start), then reload.", p.Title, p.Name)
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><meta name=viewport content=\"width=device-width\">"+
			"<title>%s</title><body style=\"font:16px system-ui;margin:2rem;max-width:40rem\"><h1>%s</h1><p>%s</p>",
			html.EscapeString(p.Title), html.EscapeString(p.Title)+" isn't running", html.EscapeString(msg))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = fmt.Fprintf(w, `{"error":{"code":"EXTENSION_UNAVAILABLE","message":%q}}`, msg)
}

type sessionKey struct{}

// WithSession tags a request with the hash of the browser session it came with ("" for none), so a
// WebSocket it upgrades is closed when that session ends.
func WithSession(r *http.Request, hash string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sessionKey{}, hash))
}

// ServeHTTP passes a request on. The caller has checked who sent it.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hash, _ := r.Context().Value(sessionKey{}).(string)
	if hash != "" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		w = &tracking{ResponseWriter: w, track: func(c net.Conn) net.Conn { return p.track(hash, c) }}
	}
	p.rp.ServeHTTP(w, r)
}

// Revoke closes the upgraded connections of every session live no longer reports as signed in.
func (p *Proxy) Revoke(live func(hash string) bool) {
	p.mu.Lock()
	var closing []net.Conn
	for hash, set := range p.conns {
		if live(hash) {
			continue
		}
		for c := range set {
			closing = append(closing, c)
		}
		delete(p.conns, hash)
	}
	p.mu.Unlock()
	for _, c := range closing {
		_ = c.Close()
	}
}

func (p *Proxy) track(hash string, c net.Conn) net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[hash] == nil {
		p.conns[hash] = map[net.Conn]struct{}{}
	}
	tc := &trackedConn{Conn: c}
	tc.done = func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.conns[hash], tc)
		if len(p.conns[hash]) == 0 {
			delete(p.conns, hash)
		}
	}
	p.conns[hash][tc] = struct{}{}
	return tc
}

// tracking hands the proxy's hijacked connection to track.
type tracking struct {
	http.ResponseWriter
	track func(net.Conn) net.Conn
}

func (t *tracking) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(t.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return t.track(c), rw, nil
}

func (t *tracking) Unwrap() http.ResponseWriter { return t.ResponseWriter }

type trackedConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *trackedConn) Close() error {
	c.once.Do(c.done)
	return c.Conn.Close()
}

// RedirectToSlash sends /ext/<name> to /ext/<name>/, so the page's relative URLs resolve.
func RedirectToSlash(w http.ResponseWriter, r *http.Request) {
	u := url.URL{Path: r.URL.Path + "/", RawQuery: r.URL.RawQuery}
	http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
}
