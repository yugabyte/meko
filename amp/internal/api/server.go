// Package api serves the controller's REST API (yblight-controller.md §3:
// API-first, versioned in the URL path). The mutation handlers are stubs for
// now; they will dispatch to the task framework as it fills in.
package api

import (
	"context"
	"log/slog"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/yugabyte/yb-light/amp/internal/config"
	"github.com/yugabyte/yb-light/amp/internal/db"
	applog "github.com/yugabyte/yb-light/amp/internal/log"
	"github.com/yugabyte/yb-light/amp/internal/task"
)

// Server wraps the echo HTTP server.
type Server struct {
	echo   *echo.Echo
	addr   string
	logger *slog.Logger
}

// NewServer wires middleware, routes, and handlers.
func NewServer(cfg *config.Config, pool *db.Pool, store task.Store, logger *slog.Logger) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(slogLogger(logger))

	h := &handlers{pool: pool, store: store}
	registerRoutes(e, h)

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

// Start begins serving and blocks until the server stops.
func (s *Server) Start() error {
	s.logger.Info("http server listening", "addr", s.addr)
	return s.echo.Start(s.addr)
}

// Shutdown gracefully drains the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.echo.Shutdown(ctx)
}
