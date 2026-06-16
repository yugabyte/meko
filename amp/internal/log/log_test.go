package log

import (
	"context"
	"log/slog"
	"testing"

	"github.com/yugabyte/yb-light/amp/internal/config"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"WARN":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewReturnsLogger(t *testing.T) {
	for _, format := range []string{"json", "text", "unset"} {
		if got := New(config.LogConfig{Level: "info", Format: format}); got == nil {
			t.Errorf("New(format=%q) returned nil", format)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if FromContext(context.Background()) != slog.Default() {
		t.Error("FromContext on empty context should return the default logger")
	}
	logger := New(config.LogConfig{Level: "debug", Format: "text"})
	ctx := IntoContext(context.Background(), logger)
	if FromContext(ctx) != logger {
		t.Error("FromContext should return the logger stored by IntoContext")
	}
}
