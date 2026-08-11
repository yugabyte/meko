package ycp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/yugabyte/amp/controller/internal/config"
)

// fakeStore is an in-memory identityStore for the loop tests.
type fakeStore struct {
	mu    sync.Mutex
	byEnv map[string]Identity
	saved int
}

func newFakeStore() *fakeStore { return &fakeStore{byEnv: map[string]Identity{}} }

func (f *fakeStore) Load(_ context.Context, envName string) (*Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byEnv[envName]; ok {
		return &id, nil
	}
	return nil, nil
}

func (f *fakeStore) Save(_ context.Context, id Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byEnv[id.EnvName] = id
	f.saved++
	return nil
}

func testLoop(store identityStore, client *Client) *Loop {
	return NewLoop(
		config.YCPConfig{EnvName: "acme", Placement: "cloud"},
		client, store, fakeInv{}, ClusterFacts{}, "test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// On a restart the persisted status token is resumed and the enrollment endpoint
// is never called — the enrollment token is short-lived and must not be spent
// again.
func TestLoop_ResumesPersistedIdentity(t *testing.T) {
	store := newFakeStore()
	store.byEnv["acme"] = Identity{EnvName: "acme", EnvironmentID: "amp-acme", StatusToken: "ycp_sts_persisted"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("enroll must not be called on resume, but %s %s was", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	id := testLoop(store, NewClient(srv.URL, nil)).ensureIdentity(context.Background())
	if id == nil || id.StatusToken != "ycp_sts_persisted" {
		t.Fatalf("ensureIdentity = %+v, want the persisted identity", id)
	}
	if store.saved != 0 {
		t.Errorf("resume wrote the identity %d times, want 0", store.saved)
	}
}

// With no persisted identity the loop enrolls and persists what it gets back.
func TestLoop_EnrollsWhenNoIdentity(t *testing.T) {
	store := newFakeStore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"environment_id":"amp-acme","status_token":"ycp_sts_fresh"},"error":null}`))
	}))
	defer srv.Close()

	id := testLoop(store, NewClient(srv.URL, nil)).ensureIdentity(context.Background())
	if id == nil || id.EnvironmentID != "amp-acme" || id.StatusToken != "ycp_sts_fresh" {
		t.Fatalf("ensureIdentity = %+v, want the enrolled identity", id)
	}
	if got, _ := store.Load(context.Background(), "acme"); got == nil || got.StatusToken != "ycp_sts_fresh" {
		t.Errorf("enrolled identity was not persisted: %+v", got)
	}
}
