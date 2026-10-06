package telemetry

import (
	"context"
	"testing"
)

func TestNoopSafeForNilUserID(t *testing.T) {
	// The Noop client must never panic regardless of input — services
	// hand us whatever they have in scope, including empty strings on
	// unauthenticated paths that briefly touched the emitter.
	c := NewNoop()
	c.Track(context.Background(), "", "", nil)
	c.Identify(context.Background(), "", nil)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush on noop returned error: %v", err)
	}
}

func TestNewRudderstackRejectsEmptyConfig(t *testing.T) {
	if _, err := NewRudderstack(RudderstackConfig{}); err == nil {
		t.Fatal("expected error when both fields empty; got nil")
	}
	if _, err := NewRudderstack(RudderstackConfig{WriteKey: "k"}); err == nil {
		t.Fatal("expected error when data plane empty; got nil")
	}
	if _, err := NewRudderstack(RudderstackConfig{DataPlaneURL: "u"}); err == nil {
		t.Fatal("expected error when write key empty; got nil")
	}
}

func TestFakeClientRecordsCalls(t *testing.T) {
	f := NewFake()
	f.Track(context.Background(), "u1", "e1", map[string]any{"k": 1})
	f.Track(context.Background(), "u2", "e2", nil)
	if len(f.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(f.Tracks))
	}
	e1 := f.EventsOf("e1")
	if len(e1) != 1 || e1[0].UserID != "u1" || e1[0].Props["k"] != 1 {
		t.Fatalf("EventsOf(e1) mismatch: %#v", e1)
	}
	if got := f.EventsOf("missing"); len(got) != 0 {
		t.Fatalf("EventsOf(missing) should be empty, got %#v", got)
	}
}

func TestSourceFromAuthMethod(t *testing.T) {
	// Growth's funnel slices by `source`. The client hint takes
	// precedence over the auth method because meko-mcp-server's MCP
	// tools authenticate with the user's upstream Cognito token after
	// an OAuth-proxy swap — so Cognito alone can't distinguish an MCP
	// tool call from a browser session. Hint absent: Cognito → UI,
	// PAT → API, nothing → API.
	cases := []struct {
		name       string
		authMethod string
		hint       string
		want       string
	}{
		// Hint takes precedence (regardless of auth method)
		{"cognito + mcp hint lands as MCP", "cognito", "mcp", SourceMCP},
		{"cognito + admin hint lands as Admin", "cognito", "admin", SourceAdmin},
		{"cognito + ui hint stays UI", "cognito", "ui", SourceUI},
		{"PAT + mcp hint lands as MCP", "pat", "mcp", SourceMCP},
		{"PAT + admin hint lands as Admin", "pat", "admin", SourceAdmin},
		// No hint: fall back to auth method
		{"cognito no hint = UI (browser default)", "cognito", "", SourceUI},
		{"PAT no hint = API", "pat", "", SourceAPI},
		{"empty method no hint = API", "", "", SourceAPI},
		// Unknown hint ignored, falls back to auth method
		{"cognito + garbage hint = UI", "cognito", "garbage", SourceUI},
		{"PAT + garbage hint = API", "pat", "garbage", SourceAPI},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SourceFromAuthMethod(c.authMethod, c.hint); got != c.want {
				t.Errorf("SourceFromAuthMethod(%q, %q) = %q, want %q",
					c.authMethod, c.hint, got, c.want)
			}
		})
	}
}
