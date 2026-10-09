package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"meko/ycp/internal/api"
	"meko/ycp/internal/ycpclient"
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
		Failed: map[reconcile.ResourceKey]error{failedCreate: errors.New("task rejected: write token held")},
		Stuck:  map[reconcile.ResourceKey]time.Duration{rw: 12 * time.Minute},
		Permanent: map[reconcile.ResourceKey]error{
			pool: reconcile.Permanent(errors.New(`delete storage pool "p": not supported`)),
		},
	}

	byID := map[string]api.ResourceStatus{}
	for _, r := range inventory(res) {
		byID[r.ID] = r
	}

	// A verb the kind does not have: state overridden to "drift" with the reason.
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

// inventory carries each resource's backend-observed Attrs through to the
// reported status so YCP sees pg version, role, compute, etc.
func TestInventoryCopiesAttrs(t *testing.T) {
	ep := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/rw"}
	res := reconcile.Result{
		Actual: []reconcile.Actual{
			{Key: ep, Ready: true, State: "ready", Attrs: map[string]string{"role": "read_write", "address": "h:5433"}},
		},
		Failed: map[reconcile.ResourceKey]error{}, Stuck: map[reconcile.ResourceKey]time.Duration{}, Permanent: map[reconcile.ResourceKey]error{},
	}
	got := inventory(res)
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].Attrs["role"] != "read_write" || got[0].Attrs["address"] != "h:5433" {
		t.Fatalf("attrs = %v, want role+address copied through", got[0].Attrs)
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

// fakeCluster is a k8s API server that holds one Secret in memory, enough for
// the agent to persist and re-read its identity.
type fakeCluster struct {
	secret map[string]string
}

func (f *fakeCluster) client(t *testing.T) *k8s {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && f.secret == nil:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet:
			enc := map[string]string{}
			for k, v := range f.secret {
				enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": enc})
		case r.Method == http.MethodPost, r.Method == http.MethodPatch:
			var body struct {
				StringData map[string]string `json:"stringData"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.secret = body.StringData
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &k8s{host: srv.URL, tok: "sa-token", ns: "ycp", http: srv.Client()}
}

// The enrollment carries the agent's configured kind. YCP checks it against the
// environment's registered kind, so sending a fixed "amp" would let a meko agent
// attach to an amp environment (and fail every verb afterwards).
func TestEnrollCarriesTheConfiguredKind(t *testing.T) {
	var got api.EnrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":{"environment_id":"acme","environment_uuid":"u-1"}}`))
	}))
	defer srv.Close()

	cluster := &fakeCluster{}
	a := &agent{
		ycp:     ycpclient.New(srv.URL, "key", srv.Client()),
		kc:      cluster.client(t),
		envName: "acme",
		kind:    "meko",
	}
	if err := a.enrollWithRetry(t.Context(), state{}); err != nil {
		t.Fatalf("enrollWithRetry: %v", err)
	}
	if got.Kind != "meko" {
		t.Errorf("enrolled kind = %q, want meko", got.Kind)
	}
	// The kind is persisted too, and compared on resume: an operator re-pointing
	// this agent at an environment of the other kind must not have the persisted
	// identity silently accepted.
	if cluster.secret["environment-kind"] != "meko" {
		t.Errorf("persisted kind = %q, want meko", cluster.secret["environment-kind"])
	}
	if cluster.secret["environment-id"] != "acme" {
		t.Errorf("persisted id = %q, want acme", cluster.secret["environment-id"])
	}
}

// A round trip through the Secret must return what went in, kind included —
// otherwise the startup guard compares against a field that is always empty and
// never discards a stale identity.
func TestStateRoundTripsTheKind(t *testing.T) {
	cluster := &fakeCluster{}
	kc := cluster.client(t)
	want := state{EnvID: "amp-acme", EnvUUID: "u-1", EnvName: "acme", EnvKind: "amp"}
	if err := kc.writeState("reconcile-agent-state", want); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	got, err := kc.readState("reconcile-agent-state")
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if got != want {
		t.Errorf("readState = %+v, want %+v", got, want)
	}
}

// A kind mismatch is the one enroll failure that must not fall back to the
// persisted identity. It can never succeed as configured, and the persisted
// identity is exactly what a flipped ENV_KIND leaves behind — a state Secret
// written before the kind was recorded reads as a match, so the startup guard
// does not fire and only this stops the agent converging an environment whose
// enrolment was refused.
func TestEnrollKindMismatchIsFatalEvenWithPersistedState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"KIND_MISMATCH","message":"registered as meko"}}`))
	}))
	defer srv.Close()

	cluster := &fakeCluster{}
	a := &agent{
		ycp:     ycpclient.New(srv.URL, "key", srv.Client()),
		kc:      cluster.client(t),
		envName: "acme",
		kind:    "amp",
	}
	// A state Secret from before the kind was recorded: EnvKind empty, which the
	// startup guard treats as a match.
	st := state{EnvID: "acme", EnvUUID: "u-1", EnvName: "acme"}
	err := a.enrollWithRetry(t.Context(), st)
	if err == nil {
		t.Fatal("a kind mismatch must not resume the persisted identity")
	}
	if !strings.Contains(err.Error(), "ENV_KIND=amp") {
		t.Errorf("error should name the misconfiguration, got %v", err)
	}
	if a.envID != "" {
		t.Errorf("envID = %q, want the agent not to have adopted an identity", a.envID)
	}
}

// The two permanent verdicts must not arrive as one word. "drift" is what the
// console renders "this resource type cannot be changed in place" from, and that
// sentence sent an operator looking for an immutable endpoint when the controller
// had simply refused the declaration and said what to write instead
// (CLOUDGA-36992). A rejected resource an operator never created still gets its
// own synthetic row, on the same terms.
func TestInventorySeparatesRejectionFromImmutability(t *testing.T) {
	const refusal = "HTTP 400: resources.memory 4Gi is below the 8 GiB per vCPU this " +
		"control plane requires: 1 cpu needs at least 8Gi (invalid_argument)"
	rw := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/rw"}
	pool := reconcile.ResourceKey{Kind: "storage-pool", Name: "p"}
	refused := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/ro"}

	res := reconcile.Result{
		Actual: []reconcile.Actual{
			{Key: rw, Ready: true, State: "ready"},
			{Key: pool, Ready: true, State: "ready"},
		},
		Failed: map[reconcile.ResourceKey]error{},
		Stuck:  map[reconcile.ResourceKey]time.Duration{},
		Permanent: map[reconcile.ResourceKey]error{
			rw:      reconcile.Rejected(errors.New(refusal)),
			pool:    reconcile.Permanent(errors.New("delete storage pool: no cascade")),
			refused: reconcile.Rejected(errors.New("create endpoint: bad memory quantity")),
		},
	}

	byID := map[string]api.ResourceStatus{}
	for _, r := range inventory(res) {
		byID[r.ID] = r
	}

	if got := byID["endpoint/p/c/main/rw"]; got.State != "rejected" || got.Error != refusal {
		t.Errorf("rw row = %+v, want state=rejected carrying the controller's reason", got)
	}
	if got := byID["storage-pool/p"]; got.State != "drift" {
		t.Errorf("pool row = %+v, want state=drift — this one really has no verb", got)
	}
	if got := byID["endpoint/p/c/main/ro"]; got.State != "rejected" || got.Error == "" {
		t.Errorf("synthetic row = %+v, want state=rejected with a reason", got)
	}
}

// A create in flight has no backend row, so without a synthetic one the resource
// is invisible for as long as its task runs — minutes, for an endpoint.
func TestInventoryShowsACreateInFlight(t *testing.T) {
	rw := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/rw"}
	rw2 := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/rw2"}
	res := reconcile.Result{
		Actual: []reconcile.Actual{{Key: rw, Ready: true, State: "ready"}},
		Updating: map[reconcile.ResourceKey]error{
			rw:  errors.New("controller task t1 planned for generation 2"),
			rw2: errors.New("controller task t2 is creating it (running)"),
		},
	}

	byID := map[string]api.ResourceStatus{}
	for _, r := range inventory(res) {
		byID[r.ID] = r
	}
	if got := byID["endpoint/p/c/main/rw2"]; got.State != "creating" || got.Name != "rw2" || got.Error == "" {
		t.Errorf("rw2 row = %+v, want state=creating carrying the task", got)
	}
	if got := byID["endpoint/p/c/main/rw"]; got.State != "updating" {
		t.Errorf("rw row = %+v, want state=updating — a row that exists is not creating", got)
	}
}

// A backend's own reason for a non-Ready resource reaches the row: on its own
// before the resource is stuck, and appended to the stuck message after.
func TestInventoryCarriesBackendDetail(t *testing.T) {
	fresh := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/rw"}
	stuck := reconcile.ResourceKey{Kind: "endpoint", Name: "p/c/main/ro"}
	const why = "container endpoint exited with code 1"
	res := reconcile.Result{
		Actual: []reconcile.Actual{
			{Key: fresh, State: "not_ready", Detail: why},
			{Key: stuck, State: "not_ready", Detail: why},
		},
		Stuck: map[reconcile.ResourceKey]time.Duration{stuck: 6 * time.Minute},
	}
	byID := map[string]api.ResourceStatus{}
	for _, r := range inventory(res) {
		byID[r.ID] = r
	}
	if got := byID["endpoint/p/c/main/rw"]; got.State != "not_ready" || got.Error != why {
		t.Errorf("fresh row = %+v, want state not_ready and error %q", got, why)
	}
	if got := byID["endpoint/p/c/main/ro"]; !strings.HasPrefix(got.Error, "no progress:") || !strings.HasSuffix(got.Error, ": "+why) {
		t.Errorf("stuck row error = %q, want the no-progress message ending in %q", got.Error, why)
	}
}
