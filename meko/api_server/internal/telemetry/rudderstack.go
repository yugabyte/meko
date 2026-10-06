package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	rudder "github.com/rudderlabs/analytics-go/v4"
)

// RudderstackConfig is the subset of rudder.Config we expose. Missing
// WriteKey or DataPlaneURL is a config error and NewRudderstack returns
// it — the caller (router.go) treats that as "telemetry off" and falls
// back to NewNoop.
type RudderstackConfig struct {
	WriteKey     string
	DataPlaneURL string
}

// RudderstackClient wraps the analytics-go v4 SDK. The SDK queues events
// in memory and flushes on its own interval; we call Flush() at server
// shutdown so the tail is not lost.
type RudderstackClient struct {
	c rudder.Client
}

// NewRudderstack constructs a real emitter. Returns an error when
// WriteKey or DataPlaneURL is empty so the router can fall back to Noop
// with a Warn — matching how SESFromAddress="" disables invite emails.
func NewRudderstack(cfg RudderstackConfig) (Client, error) {
	if cfg.WriteKey == "" || cfg.DataPlaneURL == "" {
		return nil, fmt.Errorf("rudderstack: write_key and data_plane_url are both required")
	}
	c, err := rudder.NewWithConfig(cfg.WriteKey, rudder.Config{
		DataPlaneUrl: cfg.DataPlaneURL,
		// Logger is left at the default (writes to stderr). The SDK is
		// noisy on transient errors; if that becomes a problem we can
		// pipe it through slog with a custom rudder.Logger impl.
	})
	if err != nil {
		return nil, fmt.Errorf("rudderstack: init failed: %w", err)
	}
	return &RudderstackClient{c: c}, nil
}

// Track enqueues a Track message. Errors from the SDK are logged and
// swallowed — telemetry never blocks a caller.
func (r *RudderstackClient) Track(_ context.Context, userID, event string, props map[string]any) {
	if userID == "" || event == "" {
		return
	}
	if err := r.c.Enqueue(rudder.Track{
		UserId:     userID,
		Event:      event,
		Properties: props,
	}); err != nil {
		slog.Warn("telemetry: rudderstack track failed", "event", event, "user_id", userID, "error", err)
	}
}

// Identify enqueues an Identify message.
func (r *RudderstackClient) Identify(_ context.Context, userID string, traits map[string]any) {
	if userID == "" {
		return
	}
	if err := r.c.Enqueue(rudder.Identify{
		UserId: userID,
		Traits: traits,
	}); err != nil {
		slog.Warn("telemetry: rudderstack identify failed", "user_id", userID, "error", err)
	}
}

// Flush drains the SDK batch queue. Called from the api_server shutdown
// hook so a graceful stop delivers everything the process buffered.
func (r *RudderstackClient) Flush(_ context.Context) error {
	return r.c.Close()
}
