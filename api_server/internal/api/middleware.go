// Package api provides the Echo server, router, and middleware for the
// datapack management API.
package api

import (
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// RegisterMiddleware attaches standard middleware to the Echo instance.
func RegisterMiddleware(e *echo.Echo) {
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:   true,
		LogURI:      true,
		LogStatus:   true,
		LogLatency:  true,
		LogError:    true,
		HandleError: true,
		LogValuesFunc: func(_ echo.Context, v middleware.RequestLoggerValues) error {
			slog.Info("request",
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
				"latency", v.Latency,
				"error", v.Error,
			)
			return nil
		},
	}))
	e.Use(middleware.Recover())
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{
			echo.GET, echo.POST, echo.PATCH, echo.DELETE, echo.OPTIONS,
		},
		AllowHeaders: []string{
			echo.HeaderContentType, echo.HeaderAuthorization,
		},
	}))
}

// ManagementRateLimiter returns a rate-limiting middleware sized for
// control-plane / management endpoints (not data-plane access).
//
// Defaults: 10 req/s sustained, burst of 20, 3-minute expiry per client IP.
func ManagementRateLimiter() echo.MiddlewareFunc {
	store := middleware.NewRateLimiterMemoryStoreWithConfig(
		middleware.RateLimiterMemoryStoreConfig{
			Rate:      10,
			Burst:     20,
			ExpiresIn: 3 * time.Minute,
		},
	)
	return middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Store: store,
		IdentifierExtractor: func(c echo.Context) (string, error) {
			return c.RealIP(), nil
		},
		DenyHandler: func(c echo.Context, identifier string, err error) error {
			slog.Warn("rate limit exceeded",
				"ip", identifier,
				"path", c.Path(),
			)
			return c.JSON(429, map[string]any{
				"status": "error",
				"code":   "RATE_LIMITED",
				"error":  "too many requests — try again shortly",
			})
		},
	})
}
