// Package log builds the controller's slog logger and carries it on a context.
// All controller logging goes through log/slog.
package log

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/yugabyte/yb-amp/controller/internal/config"
)

// Canonical attribute keys, so resource logs stay queryable across components.
const (
	KeyUserID        = "user_id"
	KeyStoragePoolID = "storage_pool_id"
	KeyProjectID     = "project_id"
	KeyBranchID      = "branch_id"
	KeyEndpointID    = "endpoint_id"
	KeyTaskID        = "task_id"
	KeyStep          = "step"
	KeyRequestID     = "request_id"
)

// New constructs the root logger from config. JSON by default; text for dev.
func New(cfg config.LogConfig) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "text":
		h = slog.NewTextHandler(os.Stderr, opts)
	default:
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type ctxKey struct{}

// IntoContext returns a context carrying logger.
func IntoContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the context's logger, or the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}
