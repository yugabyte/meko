// Package api serves the controller's REST API (amp-controller.md §3:
// API-first, versioned in the URL path). The HTTP contract is generated from
// openapi_spec.yaml (see gen.go); the controllers in handlers.go implement the
// generated StrictServerInterface, and the controller also serves the spec
// itself, an interactive explorer, and an embedded admin console (ui.go) at the
// server root that drives the API from the browser.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yugabyte/yb-amp/controller/internal/config"
	"github.com/yugabyte/yb-amp/controller/internal/core"
	applog "github.com/yugabyte/yb-amp/controller/internal/log"
	"github.com/yugabyte/yb-amp/controller/internal/metrics"
	"github.com/yugabyte/yb-amp/controller/internal/task"
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
func NewServer(cfg *config.Config, backend core.Backend, tasks *task.Service, prov Provisioning, m *metrics.Metrics, logger *slog.Logger) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.HTTPErrorHandler = errorHandler(logger)

	// metricsMiddleware is the outermost middleware so it records every response,
	// including the 500 that Recover writes for a panicking handler (Recover, and
	// slogLogger's HandleError for ordinary errors, both commit the mapped status
	// via the error handler before metricsMiddleware reads it on the way out).
	e.Use(metricsMiddleware(m))
	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(slogLogger(logger))

	// Prometheus scrape target. Like the probes below, it sits outside the
	// versioned API and the OpenAPI spec; the OTel Collector scrapes it in-cluster
	// (architecture/ac-metrics.md). No-op registry handler when metrics are off.
	if m != nil {
		e.GET("/metrics", echo.WrapHandler(promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{})))
	}

	// Liveness/readiness sit outside the versioned API and the spec.
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/readyz", func(c echo.Context) error {
		ctx := c.Request().Context()
		if err := backend.Healthy(ctx); err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "database unavailable")
		}
		// When controller manages a storage controller, it is not ready to serve the
		// provisioning workflow until that controller is serving too — gate on
		// it so callers (and run-controller.sh) don't race a still-starting controller.
		if prov.SC != nil {
			if err := prov.SC.Ready(ctx); err != nil {
				return echo.NewHTTPError(http.StatusServiceUnavailable, "storage controller unavailable")
			}
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
	})

	// The OpenAPI document and the interactive explorer.
	registerDocs(e)

	// The embedded admin console (a static page that drives the API).
	registerUI(e)

	// The generated, spec-driven API. The controllers implement the strict
	// interface; the generated router binds and validates against the spec.
	impl := NewControllers(backend, tasks, prov, logger)
	RegisterHandlersWithBaseURL(e, NewStrictHandler(impl, nil), apiBaseURL)

	if cfg.Deploy.Enabled {
		logger.Info("endpoint deployment enabled", "driver", cfg.Deploy.Driver)
	} else {
		logger.Warn("endpoint API is served but the deployer is disabled; endpoint create/retire will return 501 (set --deploy)")
	}

	return &Server{echo: e, addr: cfg.ListenAddr, logger: logger}
}

// metricsMiddleware records request count and latency for every handled
// request. It labels by the matched route *template* (c.Path(), e.g.
// /api/v1/pools/:poolId) rather than the raw URI, so per-id paths collapse to a
// bounded set of label values. The scrape endpoint itself is skipped so the
// collector's polling doesn't inflate the series. A nil recorder disables it.
func metricsMiddleware(m *metrics.Metrics) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if m == nil || c.Request().URL.Path == "/metrics" {
				return next(c)
			}
			start := time.Now()
			err := next(c)

			// The inner slogLogger runs the error handler (HandleError: true) on a
			// failed request, so by here the response carries the mapped status
			// (including for wrapped/non-HTTPError errors, which the custom
			// HTTPErrorHandler resolves). Just read it — no need to touch the error.
			status := c.Response().Status
			// c.Path() is the matched route template; empty for an unmatched path
			// (404) — bucket those together rather than leaving the label blank.
			route := c.Path()
			if route == "" {
				route = "unmatched"
			}
			m.ObserveHTTP(c.Request().Method, route, status, time.Since(start))
			return err
		}
	}
}

// slogLogger emits one structured access-log record per request.
func slogLogger(logger *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		// Invoke the error handler for a failed request so the logged status (and
		// the status metricsMiddleware reads) is the one the client actually gets,
		// not 200.
		HandleError:  true,
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
		"ui", uiURL, "docs", apiBaseURL+"/docs", "spec", apiBaseURL+"/openapi.yaml")
	return s.echo.Start(s.addr)
}

// Shutdown gracefully drains the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.echo.Shutdown(ctx)
}
