package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// inventory folds each resource's reason-for-not-converging onto its row, and
// emits a row for a failed/permanent verb that has no actual resource.
func TestInventoryFoldsReasons(t *testing.T) {
	pool := reconcile.ResourceKey{Kind: "storage-pool", Name: "p"}
	rw := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/rw"}
	branch := reconcile.ResourceKey{Kind: "branch", Name: "p/c/main"}
	failedCreate := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/ro"}

	res := reconcile.Result{
		Actual: []reconcile.Actual{
			{Key: pool, Ready: true, State: "ready"},
			{Key: rw, Parent: branch, Ready: false, State: "provisioning"},
		},
		Failed:    map[reconcile.ResourceKey]error{failedCreate: errors.New("task rejected: write token held")},
		Stuck:     map[reconcile.ResourceKey]time.Duration{rw: 12 * time.Minute},
		Permanent: map[reconcile.ResourceKey]error{pool: errors.New(`delete storage pool "p": not supported`)},
	}

	byID := map[string]api.ResourceStatus{}
	for _, r := range inventory(res) {
		byID[r.ID] = r
	}

	// Permanent drift: state overridden to "drift" with the reason.
	if got := byID["storage-pool/p"]; got.State != "drift" || got.Error == "" {
		t.Errorf("pool row = %+v, want state=drift with an error", got)
	}
	// Stuck: the endpoint's row carries a no-progress reason mentioning its state.
	if got := byID["endpoint/p/c/main/rw"]; got.Error == "" || got.Parent != "branch/p/c/main" {
		t.Errorf("rw row = %+v, want a stuck error and its parent link", got)
	}
	// Failed create with no actual row: synthetic row, state=error, reason kept.
	if got := byID["endpoint/p/c/main/ro"]; got.State != "error" || got.Error == "" {
		t.Errorf("ro row = %+v, want a synthetic error row", got)
	}
}

// A clean tick reports no errors on any row.
func TestInventoryCleanHasNoErrors(t *testing.T) {
	res := reconcile.Result{
		Actual: []reconcile.Actual{{Key: reconcile.ResourceKey{Kind: "storage-pool", Name: "p"}, Ready: true, State: "ready"}},
		Failed: map[reconcile.ResourceKey]error{}, Stuck: map[reconcile.ResourceKey]time.Duration{}, Permanent: map[reconcile.ResourceKey]error{},
	}
	for _, r := range inventory(res) {
		if r.Error != "" {
			t.Errorf("clean row %s carries error %q", r.ID, r.Error)
		}
	}
}

// A binding conflict is fatal and must not be retried — retrying cannot change
// the answer.
func TestBindConflictIsFatalNotRetried(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, "bound to another YCP")
	}))
	defer srv.Close()

	err := bindControllerWithRetry(context.Background(), srv.URL, "", "http://ycp", "env", func(string, ...any) {})
	if !errors.Is(err, errBindConflict) {
		t.Fatalf("err = %v, want errBindConflict", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (a conflict must not be retried)", hits)
	}
}

// A transient failure (controller not up yet) is retried until it succeeds.
func TestBindRetriesTransientThenSucceeds(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // controller starting
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := bindControllerWithRetry(context.Background(), srv.URL, "", "http://ycp", "env", func(string, ...any) {}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2 (one transient, then success)", hits)
	}
}
