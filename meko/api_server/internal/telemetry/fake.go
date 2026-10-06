package telemetry

import (
	"context"
	"sync"
)

// FakeClient captures every Track/Identify/Flush call for assertion in
// service tests. Lives in a non-test file (not fake_test.go) so the
// service package can import it — a `_test.go` symbol is compiled only
// into its own package's test binary.
type FakeClient struct {
	mu      sync.Mutex
	Tracks  []FakeTrack
	Idents  []FakeIdent
	Flushed int
}

// FakeTrack is one recorded Track call.
type FakeTrack struct {
	UserID string
	Event  string
	Props  map[string]any
}

// FakeIdent is one recorded Identify call.
type FakeIdent struct {
	UserID string
	Traits map[string]any
}

// NewFake returns a new capture client that satisfies Client.
func NewFake() *FakeClient { return &FakeClient{} }

// Track records the call.
func (f *FakeClient) Track(_ context.Context, userID, event string, props map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tracks = append(f.Tracks, FakeTrack{UserID: userID, Event: event, Props: props})
}

// Identify records the call.
func (f *FakeClient) Identify(_ context.Context, userID string, traits map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Idents = append(f.Idents, FakeIdent{UserID: userID, Traits: traits})
}

// Flush records the call and returns nil.
func (f *FakeClient) Flush(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Flushed++
	return nil
}

// EventsOf returns every recorded Track call for the given event name.
// Convenience for tests that need to assert one event out of many.
func (f *FakeClient) EventsOf(event string) []FakeTrack {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeTrack, 0, len(f.Tracks))
	for _, t := range f.Tracks {
		if t.Event == event {
			out = append(out, t)
		}
	}
	return out
}
