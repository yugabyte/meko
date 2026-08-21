package ampbackend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
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
