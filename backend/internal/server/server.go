// Package server is Meko's HTTP server on 127.0.0.1: the Host, Origin and token checks, and
// the foundation's routes (replies use internal/envelope). /mcp (Options.MCP), the rest of
// /api/v1 (Options.REST) and the web UI at / (Options.UI) are mounted on it.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/callhome"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// BodyLimit caps request bodies on /api/v1. Routes outside that group (uploads, later) set their
// own limit: one installed for every route would refuse their bodies before theirs could run.
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
	Auth         string
	Logger       *slog.Logger
	Children     func() []supervisor.Status
	MemoryEngine func(ctx context.Context) memory.EngineStatus // nil reports no memory engine
	// UsageStats, if set, says whether usage stats are sent, for status.
	UsageStats func(ctx context.Context) (callhome.State, error)
	// MCP, if set, serves /mcp (internal/mcp), with a body limit of MCPBodyLimit.
	MCP http.Handler
	// REST, if set, adds the /api/v1 routes meko_ui calls (internal/rest) to the group.
	REST func(*echo.Group)
	// Upload and Download, if set, serve the knowledge-file links (memory.UploadTicketPath and
	// DownloadTicketPath, then the ticket). They take no token: the ticket is the credential, as a
	// presigned URL's signature is. Uploads have UploadBodyLimit.
	Upload, Download echo.HandlerFunc
	// UI, if set, serves the web UI (internal/webui) on every GET no other route takes. Its files
	// need no token: the page sends the client token it was opened with on its API calls.
	UI http.Handler
}

// UploadBodyLimit caps an upload link's body: a 50 MiB file and its multipart framing.
const UploadBodyLimit = "51MiB"

// Ticket routes, which requireToken lets in without a credential, as it does the UI's.
var (
	uiRoute       = "/*"
	uploadRoute   = memory.UploadTicketPath + ":ticket"
	downloadRoute = memory.DownloadTicketPath + ":ticket"
)

// MCPBodyLimit caps request bodies on /mcp: room for a 5 MiB artifact in base64.
const MCPBodyLimit = "8M"

// Server is the HTTP server.
type Server struct {
	e     *echo.Echo
	opts  Options
	srv   *http.Server
	codes *uiCodes
}

// New builds the server and its routes.
func New(opts Options) *Server {
	if opts.Children == nil {
		opts.Children = func() []supervisor.Status { return nil }
	}
	if opts.Auth == "" {
		opts.Auth = config.AuthToken
	}
	if opts.MemoryEngine == nil {
		opts.MemoryEngine = func(context.Context) memory.EngineStatus {
			return memory.EngineStatus{State: "unavailable", Reason: "no memory engine in this server"}
		}
	}
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.Logger.SetOutput(io.Discard)
	e.HTTPErrorHandler = envelope.ErrorHandler(opts.Logger)
	s := &Server{e: e, opts: opts, codes: &uiCodes{codes: map[string]time.Time{}}}

	e.Use(middleware.RequestID())
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{
		LogErrorFunc: func(c echo.Context, err error, stack []byte) error {
			opts.Logger.Error("panic", "route", c.Path(), "error", err, "stack", string(stack))
			return err
		},
	}))
	e.Use(s.accessLog)
	e.Use(s.guard)
	e.Use(s.requireToken)

	e.GET("/healthz", s.healthz)
	v1 := e.Group("/api/v1", middleware.BodyLimit(BodyLimit))
	v1.GET("/status", s.status)
	v1.GET("/version", s.version)
	v1.POST("/ui-code", s.issueUICode)
	v1.POST("/ui-code/redeem", s.redeemUICode)
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
	if opts.UI != nil {
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
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("port %d is already in use. Stop what's using it, or pick another port "+
			"with --port, MEKO_PORT or [server] port in config.toml", port)
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
// from another site's page (an Origin header that isn't this server's own): only the web UI
// Meko serves may call it from a browser. There are no CORS headers, so no other page can read a
// reply either.
func (s *Server) guard(next echo.HandlerFunc) echo.HandlerFunc {
	port := strconv.Itoa(s.opts.Port)
	allowed := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	return func(c echo.Context) error {
		r := c.Request()
		if !allowed[r.Host] {
			return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
				"Requests must be addressed to 127.0.0.1:"+port+" or localhost:"+port+".")
		}
		if o := r.Header.Get(echo.HeaderOrigin); o != "" && (!strings.HasPrefix(o, "http://") ||
			!allowed[strings.TrimPrefix(o, "http://")]) {
			return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
				"Only Meko's own web UI may call it from a browser.")
		}
		return next(c)
	}
}

// ui serves the web UI for any GET no other route takes.
func (s *Server) ui(c echo.Context) error {
	s.opts.UI.ServeHTTP(c.Response(), c.Request())
	return nil
}

// Credentials a request can carry, as Credential reports them.
const (
	CredentialAdmin  = "admin"  // the admin token: the CLI
	CredentialClient = "client" // the client token: MCP and REST clients
	CredentialNone   = "none"   // nothing, with [server] auth = "none"
	CredentialTicket = "ticket" // a knowledge-file link's ticket, on its route only
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
// describe the server itself.
var adminOnly = map[string]bool{"/api/v1/status": true, "/api/v1/version": true, uiCodeRoute: true}

// requireToken lets a request in by its credential: /healthz needs none, nor do the knowledge-file
// links (their ticket is the credential), the web UI's files, or the UI code's redeem (its code
// is); the admin-only routes need the admin token; every other route takes the admin or the client
// token, or nothing when auth is "none". It keys on the matched route, never the raw URL.
func (s *Server) requireToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		switch c.Path() {
		case "/healthz":
			return next(c)
		case uploadRoute, downloadRoute:
			c.Set(credentialKey, CredentialTicket)
			return next(c)
		case uiRoute, uiRedeemRoute: // static files, and the one-time code that is its own credential
			return next(c)
		}
		h := c.Request().Header.Get(echo.HeaderAuthorization)
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
			c.Response().Header().Set(echo.HeaderWWWAuthenticate, "Bearer")
			return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
				"Missing or wrong token. Send the client token (meko token prints it) as Authorization: Bearer <token>.")
		}
		return next(c)
	}
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
	MemoryEngine  memory.EngineStatus `json:"memory_engine"`
	Auth          string              `json:"auth"` // token, or none
	UsageStats    *callhome.State     `json:"usage_stats,omitempty"`
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
		MemoryEngine: s.opts.MemoryEngine(c.Request().Context()), Auth: s.opts.Auth,
	}
	if s.opts.UsageStats != nil {
		if st, err := s.opts.UsageStats(c.Request().Context()); err == nil {
			reply.UsageStats = &st
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
