package ampbackend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// builtinCatalog is the builtin half every fake controller serves: the names the
// tests' clusters and branches declare, so the catalog pre-check passes them.
var builtinCatalog = []ampclient.Extension{
	{Name: "pgvector", Image: "quay.io/x/amp-ext-pgvector:1", Builtin: true},
	{Name: "pg_cron", Image: "quay.io/x/amp-ext-pg_cron:1", Builtin: true},
	{Name: "age", Image: "quay.io/x/amp-ext-age:1", Builtin: true},
}

// serveCatalog answers GET /extensions with builtinCatalog plus registered, and
// reports whether it did. Every fake controller calls it first, because List
// reads the catalog on every tick.
func serveCatalog(w http.ResponseWriter, r *http.Request, registered ...ampclient.Extension) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/extensions" {
		return false
	}
	all := append(slices.Clone(builtinCatalog), registered...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ampclient.ExtensionList{Count: len(all), Total: len(all), Extensions: all})
	return true
}

func catalogBackend(t *testing.T, status int, body string) *Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/extensions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestExtensionCatalogMapsTheConsoleFields(t *testing.T) {
	b := catalogBackend(t, http.StatusOK, `{"total":2,"count":2,"extensions":[
		{"name":"pgvector","image":"quay.io/x/pgvector:1","builtin":true,"preload":false,"version":"0.8.0","description":"vector search"},
		{"name":"my_ext","image":"quay.io/acme/my_ext:2","builtin":false,"preload":true,"library":"my_ext"}]}`)
	got, err := b.ExtensionCatalog(context.Background())
	if err != nil {
		t.Fatalf("ExtensionCatalog: %v", err)
	}
	want := []api.Extension{
		{Name: "pgvector", Builtin: true, Version: "0.8.0", Description: "vector search"},
		{Name: "my_ext", Preload: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An empty catalog is a fact to relay, so it must come back non-nil: nil is what
// the wire reserves for "unknown".
func TestExtensionCatalogEmptyIsNotNil(t *testing.T) {
	b := catalogBackend(t, http.StatusOK, `{"total":0,"count":0,"extensions":[]}`)
	got, err := b.ExtensionCatalog(context.Background())
	if err != nil {
		t.Fatalf("ExtensionCatalog: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty non-nil slice", got)
	}
}

// A controller that cannot serve its catalog must not read as one with no
// extensions — the console would then offer nothing instead of falling back.
func TestExtensionCatalogUnavailableIsAnError(t *testing.T) {
	b := catalogBackend(t, http.StatusServiceUnavailable,
		`{"error":{"code":"UNAVAILABLE","message":"catalog store not ready"}}`)
	got, err := b.ExtensionCatalog(context.Background())
	if err == nil {
		t.Fatalf("got %+v and no error, want an error", got)
	}
	if got != nil {
		t.Fatalf("got %+v alongside the error, want nil", got)
	}
}

const extPoolID = "22222222-2222-2222-2222-222222222222"

// extServer is a fake controller for the extension kind: a catalog with a
// scriptable registered half, one empty pool, and the register/deregister verbs
// answering with scriptable statuses.
type extServer struct {
	registered  []ampclient.Extension
	catalogDown bool
	putStatus   int // default 200
	delStatus   int // default 204
	calls       []string
	puts        []ampclient.RegisterExtensionRequest
}

func (s *extServer) start(t *testing.T) *Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			s.calls = append(s.calls, r.Method+" "+p)
		}
		if s.catalogDown && p == "/extensions" {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":"unavailable","message":"catalog store not ready"}}`)
			return
		}
		if serveCatalog(w, r, s.registered...) {
			return
		}
		switch {
		case p == "/users":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"`+ycpAdminUserID.String()+`","is_admin":true,"created_at":"2026-01-01T00:00:00Z"}`)
		case strings.HasPrefix(p, "/extensions/") && r.Method == http.MethodPut:
			if got := r.URL.Query().Get("user_id"); got != ycpAdminUserID.String() {
				t.Errorf("PUT user_id = %q, want %s", got, ycpAdminUserID)
			}
			var req ampclient.RegisterExtensionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode register body: %v", err)
			}
			s.puts = append(s.puts, req)
			writeStatus(w, s.putStatus, http.StatusOK, `{"name":"x","image":"`+req.Image+`","builtin":false,"preload":false}`)
		case strings.HasPrefix(p, "/extensions/") && r.Method == http.MethodDelete:
			writeStatus(w, s.delStatus, http.StatusNoContent, "")
		case strings.HasSuffix(p, "/clusters") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{}`)
		case strings.HasSuffix(p, "/clusters"):
			io.WriteString(w, `{"clusters":[]}`)
		case p == "/version":
			io.WriteString(w, `{"version":"0.0.0","git_commit":"abc1234","build_date":"2026-01-01T00:00:00Z"}`)
		case p == "/storage-pools":
			io.WriteString(w, `{"storage_pools":[{"id":"`+extPoolID+`","name":"pool-a","owner_user_id":"11111111-1111-1111-1111-111111111111","created_at":"2026-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	return b
}

func writeStatus(w http.ResponseWriter, status, def int, okBody string) {
	if status == 0 {
		status = def
	}
	w.WriteHeader(status)
	if status < 300 {
		io.WriteString(w, okBody)
		return
	}
	io.WriteString(w, `{"error":{"code":"refused","message":"the controller said no"}}`)
}

func postgisRes(spec string) api.DesiredResource {
	return api.DesiredResource{Kind: KindExtension, Name: "postgis", Spec: json.RawMessage(spec)}
}

const postgisSpec = `{"image":"quay.io/acme/postgis:3.5","version":"3.5","layout":{"share_path":"share"}}`

func postgisRow() ampclient.Extension {
	v, share := "3.5", "share"
	return ampclient.Extension{Name: "postgis", Image: "quay.io/acme/postgis:3.5", Version: &v,
		Layout: &ampclient.ExtensionLayout{SharePath: &share}}
}

func permanentReason(t *testing.T, err error) (reconcile.PermanentReason, bool) {
	t.Helper()
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		return 0, false
	}
	return perm.Reason, true
}

// Builtins ship with the release and cannot be registered or removed, so they are
// not resources — otherwise every environment would be told to deregister them.
func TestListReportsOnlyRegisteredExtensions(t *testing.T) {
	b := (&extServer{registered: []ampclient.Extension{postgisRow()}}).start(t)
	actual, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []reconcile.Actual
	for _, a := range actual {
		if a.Key.Kind == KindExtension {
			got = append(got, a)
		}
	}
	if len(got) != 1 || got[0].Key.Name != "postgis" {
		t.Fatalf("extension rows = %+v, want just postgis", got)
	}
	if got[0].ObservedGen != reconcile.GenerationNever || !got[0].Ready {
		t.Errorf("postgis row = %+v, want Ready with GenerationNever", got[0])
	}
	if got[0].Attrs["image"] != "quay.io/acme/postgis:3.5" {
		t.Errorf("attrs = %v, want the registered image", got[0].Attrs)
	}
}

// A catalog that cannot be read must not look empty: an absent row is what the
// engine creates from, and a short list hides what a withdrawal should collect.
func TestUnreadableCatalogNamesWhatItSawBefore(t *testing.T) {
	s := &extServer{registered: []ampclient.Extension{postgisRow()}}
	b := s.start(t)
	s.catalogDown = true
	actual, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List = %v, want the tree with an unreadable extension row", err)
	}
	var found bool
	for _, a := range actual {
		if a.Key == (reconcile.ResourceKey{Kind: KindExtension, Name: "postgis"}) {
			found = a.DiscoveryError != nil
		}
	}
	if !found {
		t.Fatalf("actual = %+v, want postgis carrying a DiscoveryError", actual)
	}
}

func TestRegisterExtensionMaterializesTheAdminOnce(t *testing.T) {
	s := &extServer{}
	b := s.start(t)
	for range 2 {
		if err := b.Create(t.Context(), postgisRes(postgisSpec)); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	want := []string{"POST /users", "PUT /extensions/postgis", "PUT /extensions/postgis"}
	if !reflect.DeepEqual(s.calls, want) {
		t.Fatalf("calls = %v, want %v", s.calls, want)
	}
	if s.puts[0].Image != "quay.io/acme/postgis:3.5" || s.puts[0].Layout == nil || *s.puts[0].Layout.SharePath != "share" {
		t.Errorf("PUT body = %+v, want the declared spec", s.puts[0])
	}
}

// The controller omits empty fields on read, so a spec spelling them out must
// still read as converged — or the agent would PUT on every tick.
func TestRegisteredExtensionMatchingTheSpecIssuesNothing(t *testing.T) {
	s := &extServer{registered: []ampclient.Extension{postgisRow()}}
	b := s.start(t)
	spec := `{"image":"quay.io/acme/postgis:3.5","version":"3.5","description":"","preload":false,` +
		`"abi_extra_exempt":[],"layout":{"share_path":"share","lib_path":"","env_paths":{}}}`
	if err := b.Update(t.Context(), postgisRes(spec)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("calls = %v, want none", s.calls)
	}
}

func TestRegisterExtensionVerdicts(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string // "" = retryable
	}{
		{http.StatusBadRequest, "rejected"},
		{http.StatusConflict, "rejected"},
		{http.StatusForbidden, "blocked"},
		{http.StatusInternalServerError, ""},
	} {
		b := (&extServer{putStatus: tc.status}).start(t)
		err := b.Create(t.Context(), postgisRes(postgisSpec))
		if err == nil {
			t.Fatalf("%d: Create = nil, want an error", tc.status)
		}
		reason, perm := permanentReason(t, err)
		switch {
		case tc.want == "" && perm:
			t.Errorf("%d: %v is permanent, want retryable", tc.status, err)
		case tc.want != "" && (!perm || reason.State() != tc.want):
			t.Errorf("%d: %v, want permanent %s", tc.status, err, tc.want)
		}
	}
}

func TestRegisterExtensionRefusesBeforeSending(t *testing.T) {
	for name, res := range map[string]api.DesiredResource{
		"builtin name":  {Kind: KindExtension, Name: "pgvector", Spec: json.RawMessage(postgisSpec)},
		"missing image": postgisRes(`{"version":"3.5"}`),
		"path name":     {Kind: KindExtension, Name: "a/b", Spec: json.RawMessage(postgisSpec)},
		"padded name":   {Kind: KindExtension, Name: " postgis", Spec: json.RawMessage(postgisSpec)},
		"padded image":  postgisRes(`{"image":"quay.io/acme/postgis:3.5 "}`),
	} {
		s := &extServer{}
		b := s.start(t)
		err := b.Create(t.Context(), res)
		if reason, perm := permanentReason(t, err); !perm || reason != reconcile.ReasonRejected {
			t.Errorf("%s: Create = %v, want rejected", name, err)
		}
		if len(s.calls) != 0 {
			t.Errorf("%s: calls = %v, want none", name, s.calls)
		}
	}
}

func TestDeregisterExtension(t *testing.T) {
	key := reconcile.ResourceKey{Kind: KindExtension, Name: "postgis"}
	for _, tc := range []struct {
		status int
		want   string // "ok" or "retry"
	}{
		{http.StatusNoContent, "ok"},
		{http.StatusNotFound, "ok"},
		// Still enabled on a branch: retried, since the branches may be going too,
		// and a permanent mark on a delete would never retire.
		{http.StatusConflict, "retry"},
		{http.StatusInternalServerError, "retry"},
	} {
		s := &extServer{registered: []ampclient.Extension{postgisRow()}, delStatus: tc.status}
		b := s.start(t)
		err := b.Delete(t.Context(), key)
		_, perm := permanentReason(t, err)
		switch tc.want {
		case "ok":
			if err != nil {
				t.Errorf("%d: Delete = %v, want nil", tc.status, err)
			}
		case "retry":
			if err == nil || perm {
				t.Errorf("%d: Delete = %v, want a retryable error", tc.status, err)
			}
		}
	}

	s := &extServer{}
	b := s.start(t)
	if err := b.Delete(t.Context(), key); err != nil || len(s.calls) != 0 {
		t.Fatalf("Delete of an unregistered extension = %v with calls %v, want nil and none", err, s.calls)
	}
}

// A cluster declared with an extension registered in the same write must wait
// for it rather than be sent: the controller's 400 would latch it rejected.
func TestClusterCreateWaitsForAnUnregisteredExtension(t *testing.T) {
	cluster := api.DesiredResource{Kind: KindCluster, Name: "pool-a/gis",
		Spec: json.RawMessage(`{"pg_version":18,"extensions":["pgvector","postgis"]}`)}

	s := &extServer{}
	b := s.start(t)
	err := b.Create(t.Context(), cluster)
	if err == nil || !strings.Contains(err.Error(), "postgis") {
		t.Fatalf("Create = %v, want an error naming postgis", err)
	}
	if _, perm := permanentReason(t, err); perm {
		t.Fatalf("Create = %v is permanent, want retryable", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("calls = %v, want none", s.calls)
	}

	s.registered = []ampclient.Extension{postgisRow()}
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Create(t.Context(), cluster); err != nil {
		t.Fatalf("Create once registered = %v, want nil", err)
	}
}

// Through the engine, because the bug a permanent verdict would cause lives there:
// a delete is planned at generation 0 on every tick, so a mark on one never
// retires. A refusal while a branch still enables the extension must be retried
// until the branch goes, and must not hold back a re-declaration either.
func TestRefusedDeregisterIsRetriedUntilItLands(t *testing.T) {
	s := &extServer{registered: []ampclient.Extension{postgisRow()}, delStatus: http.StatusConflict}
	b := s.start(t)
	eng := reconcile.New(b, reconcile.Options{BaseBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond})
	key := reconcile.ResourceKey{Kind: KindExtension, Name: "postgis"}

	res, err := eng.Reconcile(t.Context(), nil, true)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, failed := res.Failed[key]; !failed {
		t.Fatalf("a deregister refused for a branch using it: Failed = %v, Permanent = %v; want it failed and retryable",
			res.Failed, res.Permanent)
	}

	s.delStatus = http.StatusNoContent // the branch is gone
	if _, err := eng.Reconcile(t.Context(), nil, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := s.calls[len(s.calls)-1]; got != "DELETE /extensions/postgis" {
		t.Fatalf("last call = %q, want the deregister retried", got)
	}

	// Declared again at the generation a fresh row starts at: the refusal must not
	// still be holding the resource.
	s.delStatus = http.StatusConflict
	s.calls = nil
	eng = reconcile.New(b, reconcile.Options{BaseBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond})
	if _, err := eng.Reconcile(t.Context(), nil, true); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res, err = eng.Reconcile(t.Context(), []api.DesiredResource{postgisRes(postgisSpec)}, true)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, held := res.Permanent[key]; held {
		t.Fatalf("re-declared extension is held by a stale verdict: %v", res.Permanent[key])
	}
	if _, failed := res.Failed[key]; failed {
		t.Fatalf("re-declared extension failed: %v", res.Failed[key])
	}
}

// A branch naming an extension not registered yet still gets its settings, but
// claims no generation: the declared set has not landed, so the next tick must
// come back for it.
func TestBranchWaitingOnTheCatalogStillAppliesSettings(t *testing.T) {
	b, got := listedTree(t, []string{"pgvector"})
	err := b.Update(t.Context(), branchResGen(
		`{"extensions":["pgvector","postgis"],"pg_settings":{"work_mem":"64MB"}}`, 3))
	if err == nil || !strings.Contains(err.Error(), "postgis") {
		t.Fatalf("Update = %v, want an error naming postgis", err)
	}
	if _, perm := permanentReason(t, err); perm {
		t.Fatalf("Update = %v is permanent, want retryable", err)
	}
	if len(got.added) != 0 {
		t.Errorf("added %v, want nothing until postgis is registered", got.added)
	}
	if len(got.patched) != 1 {
		t.Fatalf("settings patches = %d, want 1", len(got.patched))
	}
	if g := got.patched[0].YcpGeneration; g != nil {
		t.Errorf("settings patch claimed generation %d, want none", *g)
	}
}
