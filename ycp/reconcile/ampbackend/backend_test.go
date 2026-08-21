package ampbackend

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// A pool declares its owner via owner_user_id, but users are not a YCP kind, so
// the agent must materialize that owner before creating the pool — otherwise the
// controller 404s on a missing owner on a fresh deploy.
func TestCreatePoolEnsuresOwnerFirst(t *testing.T) {
	const ownerID = "11111111-1111-1111-1111-111111111111"

	var hits []string
	var userBody ampclient.CreateUserRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/users":
			if err := json.NewDecoder(r.Body).Decode(&userBody); err != nil {
				t.Errorf("decode user body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"` + ownerID + `","is_admin":false}`))
		case "/storage-pools":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"22222222-2222-2222-2222-222222222222"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = b.Create(t.Context(), api.DesiredResource{
		Kind: KindPool,
		Name: "pool-a",
		Spec: json.RawMessage(`{"owner_user_id":"` + ownerID + `"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []string{"POST /users", "POST /storage-pools"}
	if len(hits) != len(want) {
		t.Fatalf("request sequence = %v, want %v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("request sequence = %v, want %v", hits, want)
		}
	}
	if userBody.Id == nil || userBody.Id.String() != ownerID {
		t.Fatalf("ensure-user id = %v, want %s", userBody.Id, ownerID)
	}
}

// With no owner in the spec the agent skips the user create and lets the
// controller reject the pool with its own required-field error.
func TestCreatePoolWithoutOwnerSkipsUserCreate(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Create(t.Context(), api.DesiredResource{
		Kind: KindPool,
		Name: "pool-a",
		Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(hits) != 1 || hits[0] != "POST /storage-pools" {
		t.Fatalf("request sequence = %v, want [POST /storage-pools]", hits)
	}
}

// List reports each resource's kind-specific details in Attrs so YCP can render
// pg version, LSN, extensions, role and compute — not just topology and state.
func TestListReportsResourceAttrs(t *testing.T) {
	const (
		poolID     = "22222222-2222-2222-2222-222222222222"
		clusterID  = "33333333-3333-3333-3333-333333333333"
		branchID   = "44444444-4444-4444-4444-444444444444"
		endpointID = "55555555-5555-5555-5555-555555555555"
		parentID   = "66666666-6666-6666-6666-666666666666"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/endpoints"):
			io.WriteString(w, `{"endpoints":[{"id":"`+endpointID+`","name":"rw","branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","role":"read_write","address":"ep.host:5433","tailnet_address":"ep.tail:5433","resources":{"cpu":"2","memory":"4Gi"},"created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/branches"):
			io.WriteString(w, `{"branches":[{"branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","name":"main","is_default":false,"pg_version":18,"fork_lsn":"1/2","extensions":["pgvector","pg_cron"],"parent_branch_id":"`+parentID+`","created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/clusters"):
			io.WriteString(w, `{"clusters":[{"id":"`+clusterID+`","name":"recproj","storage_pool_id":"`+poolID+`","created_at":"2026-01-01T00:00:00Z","default_branch":{"branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","name":"main","is_default":true,"pg_version":18,"created_at":"2026-01-01T00:00:00Z"}}]}`)
		case strings.HasSuffix(p, "/storage-pools"):
			io.WriteString(w, `{"storage_pools":[{"id":"`+poolID+`","name":"pool-a","owner_user_id":"11111111-1111-1111-1111-111111111111","created_at":"2026-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKind := map[string]reconcile.Actual{}
	for _, a := range actuals {
		byKind[a.Key.Kind] = a
	}

	cl := byKind[KindCluster].Attrs
	if cl["id"] != clusterID || cl["pg_version"] != "18" {
		t.Errorf("cluster attrs = %v", cl)
	}

	br := byKind[KindBranch].Attrs
	if br["id"] != branchID || br["pg_version"] != "18" || br["fork_lsn"] != "1/2" ||
		br["extensions"] != "pgvector,pg_cron" || br["parent_branch_id"] != parentID {
		t.Errorf("branch attrs = %v", br)
	}
	if _, ok := br["is_default"]; ok {
		t.Errorf("branch is_default should be absent when false, got %v", br)
	}

	ep := byKind[KindEndpoint].Attrs
	if ep["id"] != endpointID || ep["role"] != "read_write" || ep["address"] != "ep.host:5433" ||
		ep["tailnet_address"] != "ep.tail:5433" || ep["cpu"] != "2" || ep["memory"] != "4Gi" {
		t.Errorf("endpoint attrs = %v", ep)
	}

	if a := byKind[KindPool].Attrs; a["id"] != poolID {
		t.Errorf("pool attrs = %v, want id %s", a, poolID)
	}
}
