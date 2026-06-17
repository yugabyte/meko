// Package api serves the controller's REST API (yblight-controller.md §3:
// API-first, versioned in the URL path). The HTTP contract is generated from
// openapi_spec.yaml (see gen.go); the controllers in handlers.go implement the
// generated StrictServerInterface, and the controller also serves the spec
// itself and an interactive explorer.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/yugabyte/yb-light/amp/internal/config"
	"github.com/yugabyte/yb-light/amp/internal/core"
	applog "github.com/yugabyte/yb-light/amp/internal/log"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

// apiBaseURL is where the generated routes are mounted (the spec's server URL).
const apiBaseURL = "/api/v1"

// Server wraps the echo HTTP server.
type Server struct {
	echo   *echo.Echo
	addr   string
	logger *slog.Logger
}

// NewServer wires middleware, the spec/explorer routes, and the generated,
// spec-driven API onto an echo server.
func NewServer(cfg *config.Config, backend core.Backend, store task.Store, prov Provisioning, logger *slog.Logger) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.HTTPErrorHandler = errorHandler(logger)

	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(slogLogger(logger))

	// Liveness/readiness sit outside the versioned API and the spec.
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/readyz", func(c echo.Context) error {
		ctx := c.Request().Context()
		if err := backend.Healthy(ctx); err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "database unavailable")
		}
		// When amp manages a storage controller, it is not ready to serve the
		// provisioning workflow until that controller is serving too — gate on
		// it so callers (and run-amp.sh) don't race a still-starting controller.
		if prov.SC != nil {
			if err := prov.SC.Ready(ctx); err != nil {
				return echo.NewHTTPError(http.StatusServiceUnavailable, "storage controller unavailable")
			}
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
	})

	// The OpenAPI document and the interactive explorer.
	registerDocs(e)

	// The generated, spec-driven API. The controllers implement the strict
	// interface; the generated router binds and validates against the spec.
	impl := NewControllers(backend, store, prov, logger)
	RegisterHandlersWithBaseURL(e, NewStrictHandler(impl, nil), apiBaseURL)

	return &Server{echo: e, addr: cfg.ListenAddr, logger: logger}
}

// slogLogger emits one structured access-log record per request.
func slogLogger(logger *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus:    true,
		LogMethod:    true,
		LogURI:       true,
		LogLatency:   true,
		LogRequestID: true,
		LogError:     true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			if v.Status >= 500 || v.Error != nil {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("method", v.Method),
				slog.String("uri", v.URI),
				slog.Int("status", v.Status),
				slog.String("latency", v.Latency.String()),
				slog.String(applog.KeyRequestID, v.RequestID),
			}
			if v.Error != nil {
				attrs = append(attrs, slog.String("error", v.Error.Error()))
			}
			logger.LogAttrs(c.Request().Context(), level, "request", attrs...)
			return nil
		},
	})
}

// Handler returns the underlying HTTP handler, for tests that drive the API
// through httptest without binding a socket.
func (s *Server) Handler() http.Handler { return s.echo }

// Start begins serving and blocks until the server stops.
func (s *Server) Start() error {
	s.logger.Info("http server listening", "addr", s.addr,
		"docs", apiBaseURL+"/docs", "spec", apiBaseURL+"/openapi.yaml")
	return s.echo.Start(s.addr)
}

// Shutdown gracefully drains the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.echo.Shutdown(ctx)
}
