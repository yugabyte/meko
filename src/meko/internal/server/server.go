// Package server is Meko's HTTP server, on its one port: agents and the CLI call /mcp (Options.MCP)
// and /api/v1 (Options.REST) with a Bearer token, and browsers (Options.Web) get the web UI at /
// (Options.UI), the same routes and the extensions' UIs at /ext/<name>/, with local or remote access
// and no login. Which credential a request has depends on the request, never on how it came in
// (authorize); the Host and Origin checks depend on the connection (guard). With remote access the
// port takes TLS from anywhere and plain HTTP from this computer (Sniff). Replies use
// internal/envelope.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/auth"
	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/supervisor"
)

// BodyLimit caps request bodies on /api/v1. Routes outside that group (/mcp, the upload link) set
// their own limit: one installed for every route would refuse their bodies before theirs could run.
const BodyLimit = "1M"

// Info is what status and version report.
type Info struct {
	Version   string
	Commit    string
	GoVersion string
	Libs      store.Versions
	DataDir   string
	PID       int
	StartedAt time.Time
}

// Options configure a Server.
type Options struct {
	Info  Info
	Port  int    // the port it listens on, for the Host check
	Token string // the admin token
	// ClientToken is the clients' token, accepted on every route but the admin-only ones.
	ClientToken string
	// Auth is config.AuthToken, or config.AuthNone to let /mcp and the data routes take requests
	// with no credential.
	Auth          string
	Logger        *slog.Logger
	Children      func() []supervisor.Status
	ContextEngine func(ctx context.Context) memory.EngineStatus // nil reports no context engine
	// UsageStats, if set, says whether usage stats are sent, for status.
	UsageStats func(ctx context.Context) (callhome.State, error)
	// Storage, if set, says how full Meko is against its storage limit, for status.
	Storage func(ctx context.Context) (memory.StorageStatus, error)
	// MCP, if set, serves /mcp (internal/mcp), with a body limit of MCPBodyLimit.
	MCP http.Handler
	// REST, if set, adds the /api/v1 routes meko_ui calls (internal/rest) to the group.
	REST func(*echo.Group)
	// Upload and Download, if set, serve the knowledge-file links (memory.UploadTicketPath and
	// DownloadTicketPath, then the ticket). They take no token: the ticket is the credential, as a
	// presigned URL's signature is. Uploads have UploadBodyLimit.
	Upload, Download echo.HandlerFunc
	// UI, if set with Web, serves the web UI (internal/webui) on every GET no other route takes. Its
	// files need no credential.
	UI http.Handler
	// Web, if set, lets browsers in too (local or remote access), and serves their routes: the
	// extensions' UIs, the Extensions page's and the Status page's. Without it only tokens get in.
	Web *Web
	// WebStatus, if set, is the state of the port browsers use, for status.
	WebStatus func() *WebStatus
	// ReloadWeb, if set, serves POST /api/v1/web/reload (admin token only), for meko ui start and
	// stop: the listener reads its settings again, and lets browsers in or keeps them out.
	ReloadWeb func()
	// Cloud, if set, serves the Meko Cloud link's routes (/api/v1/settings/meko-cloud): to the web UI
	// and to the CLI's admin token (meko cloud-link), never to the client token. The linked Meko
	// Cloud's datapacks go through the REST routes (internal/rest's cloud.go).
	Cloud *cloudlink.Service
	// Agents, if set, serves the agents' routes (/api/v1/agents, agents.go).
	Agents *agents.Manager
}

// WebStatus is what status says about the web UI and the port browsers use.
type WebStatus struct {
	URL         string `json:"url"`
	Access      string `json:"access"` // local or remote
	Listening   bool   `json:"listening"`
	Error       string `json:"error,omitempty"`       // why it isn't listening
	Fingerprint string `json:"fingerprint,omitempty"` // its certificate's SHA-256, with remote access
	SelfSigned  bool   `json:"self_signed,omitempty"`
	// Enabled is [web] enabled, as the listener has taken it up: false while meko ui stop keeps
	// browsers out (the port stays open to agents and the CLI, on 127.0.0.1).
	Enabled bool `json:"enabled"`
}

// UploadBodyLimit caps an upload link's body: a 50 MiB file and its multipart framing.
const UploadBodyLimit = "51MiB"

// Ticket routes, which the credential checks let in without a credential, as they do the UI's.
var (
	uiRoute       = "/*"
	uploadRoute   = memory.UploadTicketPath + ":ticket"
	downloadRoute = memory.DownloadTicketPath + ":ticket"
)

// MCPBodyLimit caps request bodies on /mcp: room for a 5 MiB artifact in base64.
const MCPBodyLimit = "8M"

// Server is the HTTP server.
type Server struct {
	e    *echo.Echo
	opts Options
	srv  *http.Server
}

// New builds the server and its routes.
func New(opts Options) *Server {
	if opts.Children == nil {
		opts.Children = func() []supervisor.Status { return nil }
	}
	if opts.Auth == "" {
		opts.Auth = config.AuthToken
	}
	if opts.ContextEngine == nil {
		opts.ContextEngine = func(context.Context) memory.EngineStatus {
			return memory.EngineStatus{State: "unavailable", Reason: "no context engine in this server"}
		}
	}
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.Logger.SetOutput(io.Discard)
	e.HTTPErrorHandler = envelope.ErrorHandler(opts.Logger)
	s := &Server{e: e, opts: opts}

	e.Use(middleware.RequestID())
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{
		LogErrorFunc: func(c echo.Context, err error, stack []byte) error {
			opts.Logger.Error("panic", "route", c.Path(), "error", err, "stack", string(stack))
			return err
		},
	}))
	e.Use(s.accessLog)
	e.Use(s.guard)
	e.Use(s.authorize)

	e.GET("/healthz", s.healthz)
	v1 := e.Group("/api/v1", middleware.BodyLimit(BodyLimit))
	// The same reply as status for the web UI's status page: what it says (versions, the context
	// engine's state, the data folder) is the local user's own.
	v1.GET("/server", s.status)
	v1.GET("/status", s.status)
	v1.GET("/version", s.version)
	if opts.ReloadWeb != nil {
		v1.POST("/web/reload", s.reloadWeb)
	}
	s.registerCloud(v1)
	if opts.Web != nil {
		if opts.Web.Extensions == nil {
			opts.Web.Extensions = func() []*extproxy.Proxy { return nil }
		}
		if opts.Web.Jobs == nil {
			opts.Web.Jobs = NewExtensionJobs()
		}
		s.registerWeb(v1)
	}
	s.registerAgents(v1)
	if opts.REST != nil {
		opts.REST(v1)
	}
	if opts.Upload != nil {
		e.POST(uploadRoute, opts.Upload, middleware.BodyLimit(UploadBodyLimit))
	}
	if opts.Download != nil {
		e.GET(downloadRoute, opts.Download)
	}
	if opts.MCP != nil {
		// The handler reads the credential from the request's context, outside echo.
		e.Any("/mcp", func(c echo.Context) error {
			r := c.Request()
			r = r.WithContext(context.WithValue(r.Context(), credentialCtxKey{}, Credential(c)))
			opts.MCP.ServeHTTP(c.Response(), r)
			return nil
		}, middleware.BodyLimit(MCPBodyLimit))
	}
	if opts.UI != nil && opts.Web != nil {
		// The UI takes every GET no other route does. These keep the API's unknown paths behind
		// the token and answering 404 in the envelope, so uiRoute matches only the UI's.
		notFound := func(echo.Context) error { return echo.ErrNotFound }
		for _, p := range []string{"/api", "/api/*", "/mcp/*"} {
			e.Any(p, notFound)
		}
		e.GET(uiRoute, s.ui)
	}
	s.srv = &http.Server{Handler: e, ReadHeaderTimeout: 10 * time.Second}
	return s
}

// ServeHTTP lets tests drive the server without a listener.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.e.ServeHTTP(w, r) }

// Serve answers requests on ln until Shutdown. It returns nil after a shutdown.
func (s *Server) Serve(ln net.Listener) error {
	if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops accepting connections and waits for requests in flight, until ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Listen opens 127.0.0.1:port. A taken port gets an error saying how to pick another.
func Listen(port int) (net.Listener, error) {
	return ListenOn("127.0.0.1", port)
}

// ListenOn opens host:port ("" is every address). A taken port gets an error saying what may hold
// it and how to pick another.
func ListenOn(host string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("port %d is in use (a Kode from before Meko served it, or another program). "+
			"Stop it, or pick another port with --port, MEKO_PORT or [server] port in config.toml", port)
	}
	return ln, err
}

func (s *Server) accessLog(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		if err := next(c); err != nil {
			c.Error(err)
		}
		s.opts.Logger.Info("request", "method", c.Request().Method, "route", c.Path(),
			"status", c.Response().Status, "ms", time.Since(start).Milliseconds(),
			"request_id", c.Response().Header().Get(echo.HeaderXRequestID))
		return nil
	}
}

// guard refuses requests that don't name this server in Host (DNS rebinding) and any request
// from another site's page (an Origin header that isn't this server's own): only the pages Meko
// serves may call it from a browser. There are no CORS headers, so no other page can read a reply
// either. Plain HTTP, which comes only from this computer, must name 127.0.0.1, localhost or [::1]
// with this port, and an Origin must be http:// one of those. TLS (remote access) has no Host list,
// as Kode had none: a rebound page can't pass TLS for another name. Its Origin must be
// https://<Host>.
func (s *Server) guard(next echo.HandlerFunc) echo.HandlerFunc {
	port := strconv.Itoa(s.opts.Port)
	allowed := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true}
	return func(c echo.Context) error {
		r := c.Request()
		overTLS := r.TLS != nil
		if !overTLS && !allowed[r.Host] {
			return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
				"Requests must be addressed to 127.0.0.1:"+port+" or localhost:"+port+".")
		}
		o := r.Header.Get(echo.HeaderOrigin)
		ownOrigin := strings.HasPrefix(o, "http://") && allowed[strings.TrimPrefix(o, "http://")]
		if overTLS {
			ownOrigin = strings.EqualFold(o, "https://"+r.Host)
		}
		if o != "" && !ownOrigin {
			return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
				"Only Meko's own web UI may call it from a browser.")
		}
		return next(c)
	}
}

// ui serves the web UI for any GET no other route takes; while it's off, a page saying so.
func (s *Server) ui(c echo.Context) error {
	if s.opts.Web.Off {
		return c.String(http.StatusNotFound, "The web UI is off. Turn it on with meko ui start.\n")
	}
	s.opts.UI.ServeHTTP(c.Response(), c.Request())
	return nil
}

// Credentials a request can carry, as Credential reports them.
const (
	CredentialAdmin  = "admin"  // the admin token: the CLI
	CredentialClient = "client" // the client token: MCP and REST clients
	CredentialNone   = "none"   // nothing, with [server] auth = "none"
	CredentialTicket = "ticket" // a knowledge-file link's ticket, on its route only
	// A browser's: a request from this computer (LocalAccess) over plain HTTP, or any same-site
	// request over TLS with remote access (SameSite).
	CredentialLocal  = "local"
	CredentialRemote = "remote"
)

const credentialKey = "meko.credential" //nolint:gosec // G101: a context key, not a secret

type credentialCtxKey struct{}

// CredentialOf is the credential a request to /mcp was let in with, for its handler.
func CredentialOf(r *http.Request) string {
	v, _ := r.Context().Value(credentialCtxKey{}).(string)
	return v
}

// Credential is the credential a request was let in with.
func Credential(c echo.Context) string {
	v, _ := c.Get(credentialKey).(string)
	return v
}

// adminOnly are the routes only the CLI's admin token opens, whatever the auth setting: they
// describe the server itself, or start and stop the web UI.
var adminOnly = map[string]bool{"/api/v1/status": true, "/api/v1/version": true, reloadWebRoute: true}

// reloadWebRoute is for meko ui start and stop.
const reloadWebRoute = "/api/v1/web/reload"

func (s *Server) reloadWeb(c echo.Context) error {
	s.opts.ReloadWeb()
	return envelope.OK(c, map[string]bool{"reloading": true})
}

// authorize is the credential check, keyed on the matched route, never the raw URL, and on what
// the request carries, never on how it came in:
//   - /healthz, the web UI's files, /ext/<name> (a redirect) and auth/status need nothing, and the
//     knowledge-file links' ticket is their credential;
//   - an extension's pages (/ext/<name>/...) take a browser (local or remote) and nothing else: a
//     token there is the extension's business, never Meko's;
//   - a request with Authorization: Bearer is held to it (checkToken), and never falls back to the
//     browser rules: only from this computer, the admin token anywhere, the client token everywhere
//     but the admin-only routes and the browsers' own (ownerOnly);
//   - one without a token is a browser's (browser) when it passes their rules, then, with auth =
//     "none", anything from this computer on the data routes that isn't a browser's (browserSent),
//     and else refused.
func (s *Server) authorize(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		switch c.Path() {
		case "/healthz", authStatusRoute, uiRoute, extRootRoute:
			return next(c)
		case uploadRoute, downloadRoute:
			c.Set(credentialKey, CredentialTicket)
			return next(c)
		case extRoute:
			if cred := s.browser(r); cred != "" {
				c.Set(credentialKey, cred)
				return next(c)
			}
			return s.refuseBrowser(c)
		}
		if h := r.Header.Get(echo.HeaderAuthorization); h != "" {
			return s.checkToken(c, next, h)
		}
		if adminOnly[c.Path()] {
			c.Response().Header().Set(echo.HeaderWWWAuthenticate, "Bearer")
			return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
				"Missing or wrong admin token. Use the meko CLI, which reads it from the data folder.")
		}
		if cred := s.browser(r); cred != "" {
			c.Set(credentialKey, cred)
			return next(c)
		}
		// auth = "none" is for programs on this computer, never a browser's way in: one the browsers'
		// rules refused (or kept out while the web UI is off) is refused here too.
		if s.opts.Auth == config.AuthNone && fromThisComputer(r) && !browserSent(r) {
			c.Set(credentialKey, CredentialNone)
			return next(c)
		}
		if r.TLS != nil || browserSent(r) {
			return s.refuseBrowser(c)
		}
		return missingToken(c)
	}
}

// checkToken holds a request with an Authorization header to it: tokens are taken only from this
// computer, whether the request came over plain HTTP or TLS.
func (s *Server) checkToken(c echo.Context, next echo.HandlerFunc, h string) error {
	r := c.Request()
	if !fromThisComputer(r) {
		return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
			"Meko takes tokens only from this computer: send them to http://127.0.0.1:"+strconv.Itoa(s.opts.Port)+".")
	}
	isClient := s.opts.ClientToken != "" && auth.CheckBearer(h, s.opts.ClientToken)
	switch {
	case auth.CheckBearer(h, s.opts.Token):
		c.Set(credentialKey, CredentialAdmin)
	case adminOnly[c.Path()] && isClient:
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"This route needs the admin token. Use the meko CLI, which reads it from the data folder.")
	case adminOnly[c.Path()]:
		c.Response().Header().Set(echo.HeaderWWWAuthenticate, "Bearer")
		return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
			"Missing or wrong admin token. Use the meko CLI, which reads it from the data folder.")
	case isClient:
		c.Set(credentialKey, CredentialClient)
	case s.opts.Auth == config.AuthNone:
		c.Set(credentialKey, CredentialNone)
	default:
		return missingToken(c)
	}
	return next(c)
}

func missingToken(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderWWWAuthenticate, "Bearer")
	return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
		"Missing or wrong token. Send the client token (meko token prints it) as Authorization: Bearer <token>.")
}

// ownerOnly is a route's check for the routes only the owner reaches: a browser, or the CLI's admin
// token. An agent's client token, or nothing with auth = "none", gets a 403: installing software,
// updating Meko or changing the Meko Cloud link is the owner's call.
func ownerOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		switch Credential(c) {
		case CredentialAdmin, CredentialLocal, CredentialRemote:
			return next(c)
		}
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"Only the web UI, or the meko CLI, may use this route.")
	}
}

// fromThisComputer reports whether a request's connection comes from a loopback address.
func fromThisComputer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(host)
	return err == nil && peer.Unmap().IsLoopback()
}

func (s *Server) healthz(c echo.Context) error {
	return envelope.OK(c, map[string]string{"status": "ok", "version": s.opts.Info.Version})
}

// StatusReply is GET /api/v1/status.
type StatusReply struct {
	Version       string              `json:"version"`
	PID           int                 `json:"pid"`
	Port          int                 `json:"port"`
	DataDir       string              `json:"data_dir"`
	StartedAt     time.Time           `json:"started_at"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	Children      []supervisor.Status `json:"children"`
	ContextEngine memory.EngineStatus `json:"context_engine"`
	Auth          string              `json:"auth"` // token, or none
	UsageStats    *callhome.State     `json:"usage_stats,omitempty"`
	Web           *WebStatus          `json:"web,omitempty"`
	// Storage is how much content Meko holds, by kind, and its limit ([storage] max_content_mb).
	Storage *memory.StorageStatus `json:"storage,omitempty"`
}

func (s *Server) status(c echo.Context) error {
	i := s.opts.Info
	children := s.opts.Children()
	if children == nil {
		children = []supervisor.Status{}
	}
	reply := StatusReply{
		Version: i.Version, PID: i.PID, Port: s.opts.Port, DataDir: i.DataDir, StartedAt: i.StartedAt,
		UptimeSeconds: int64(time.Since(i.StartedAt).Seconds()), Children: children,
		ContextEngine: s.opts.ContextEngine(c.Request().Context()), Auth: s.opts.Auth,
	}
	if s.opts.WebStatus != nil {
		reply.Web = s.opts.WebStatus()
	}
	if s.opts.UsageStats != nil {
		if st, err := s.opts.UsageStats(c.Request().Context()); err == nil {
			reply.UsageStats = &st
		}
	}
	if s.opts.Storage != nil {
		if st, err := s.opts.Storage(c.Request().Context()); err == nil {
			reply.Storage = &st
		}
	}
	return envelope.OK(c, reply)
}

// VersionReply is GET /api/v1/version.
type VersionReply struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	store.Versions
}

func (s *Server) version(c echo.Context) error {
	i := s.opts.Info
	return envelope.OK(c, VersionReply{Version: i.Version, Commit: i.Commit, GoVersion: i.GoVersion, Versions: i.Libs})
}
