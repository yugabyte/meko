// Package api provides the Echo server, router, and middleware for the
// datapack management API.
package api

import (
	"log/slog"

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
