// Package server is Meko Local's HTTP server on 127.0.0.1: the {data, error} envelope,
// the Host, Origin and admin-token checks, and the foundation's routes. /mcp, the rest of
// /api/v1 and the UI are mounted in later milestones.
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
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/yugabyte/meko-local/backend/internal/auth"
	"github.com/yugabyte/meko-local/backend/internal/store"
	"github.com/yugabyte/meko-local/backend/internal/supervisor"
)

// BodyLimit caps request bodies; upload routes will raise it for themselves.
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
	Info     Info
	Port     int    // the port it listens on, for the Host check
	Token    string // the admin token
	Logger   *slog.Logger
	Children func() []supervisor.Status
}

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
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.Logger.SetOutput(io.Discard)
	e.HTTPErrorHandler = errorHandler(opts.Logger)
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
	e.Use(s.requireToken)
	e.Use(middleware.BodyLimit(BodyLimit))

	e.GET("/healthz", s.healthz)
	v1 := e.Group("/api/v1")
	v1.GET("/status", s.status)
	v1.GET("/version", s.version)
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
			"with --port, MEKO_LOCAL_PORT or [server] port in config.toml", port)
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
// from a browser page (an Origin header): there's no browser client in this phase.
func (s *Server) guard(next echo.HandlerFunc) echo.HandlerFunc {
	port := strconv.Itoa(s.opts.Port)
	allowed := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	return func(c echo.Context) error {
		r := c.Request()
		if !allowed[r.Host] {
			return apiError(c, http.StatusForbidden, "FORBIDDEN",
				"Requests must be addressed to 127.0.0.1:"+port+" or localhost:"+port+".")
		}
		if r.Header.Get(echo.HeaderOrigin) != "" {
			return apiError(c, http.StatusForbidden, "FORBIDDEN", "Browser requests aren't accepted yet.")
		}
		return next(c)
	}
}

// requireToken checks the admin token on every route but /healthz. It keys on the matched
// route, never the raw URL.
func (s *Server) requireToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Path() == "/healthz" || auth.CheckBearer(c.Request().Header.Get(echo.HeaderAuthorization), s.opts.Token) {
			return next(c)
		}
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED",
			"Missing or wrong admin token. Use the meko-local CLI, which reads it from the data folder.")
	}
}

func (s *Server) healthz(c echo.Context) error {
	return ok(c, map[string]string{"status": "ok", "version": s.opts.Info.Version})
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
}

func (s *Server) status(c echo.Context) error {
	i := s.opts.Info
	children := s.opts.Children()
	if children == nil {
		children = []supervisor.Status{}
	}
	return ok(c, StatusReply{
		Version: i.Version, PID: i.PID, Port: s.opts.Port, DataDir: i.DataDir, StartedAt: i.StartedAt,
		UptimeSeconds: int64(time.Since(i.StartedAt).Seconds()), Children: children,
	})
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
	return ok(c, VersionReply{Version: i.Version, Commit: i.Commit, GoVersion: i.GoVersion, Versions: i.Libs})
}
