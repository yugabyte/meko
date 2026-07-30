package api

import (
	"testing"
	"time"
)

func TestEffectiveFlushInterval_DefaultWhenZero(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
	}{
		{"unset", ServerConfig{}},
		{"explicit zero", ServerConfig{MCPPATFlushInterval: 0}},
		{"negative", ServerConfig{MCPPATFlushInterval: -5 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveFlushInterval(tc.cfg)
			if got != defaultMCPPATFlushInterval {
				t.Errorf("expected default %v, got %v",
					defaultMCPPATFlushInterval, got)
			}
		})
	}
}

func TestEffectiveFlushInterval_ClampsBelowFloor(t *testing.T) {
	cases := []time.Duration{
		1 * time.Nanosecond,
		1 * time.Millisecond,
		500 * time.Millisecond,
		mcpPATFlushIntervalFloor - time.Nanosecond,
	}
	for _, d := range cases {
		t.Run(d.String(), func(t *testing.T) {
			got := effectiveFlushInterval(ServerConfig{MCPPATFlushInterval: d})
			if got != mcpPATFlushIntervalFloor {
				t.Errorf("expected clamp to floor %v, got %v",
					mcpPATFlushIntervalFloor, got)
			}
		})
	}
}

func TestEffectiveFlushInterval_PassesThroughAboveFloor(t *testing.T) {
	cases := []time.Duration{
		mcpPATFlushIntervalFloor,
		2 * time.Second,
		30 * time.Second,
		5 * time.Minute,
		1 * time.Hour,
	}
	for _, d := range cases {
		t.Run(d.String(), func(t *testing.T) {
			got := effectiveFlushInterval(ServerConfig{MCPPATFlushInterval: d})
			if got != d {
				t.Errorf("expected pass-through %v, got %v", d, got)
			}
		})
	}
}
