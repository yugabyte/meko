package mekoapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recorded struct {
	method, path, auth, body string
}

func server(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL), &seen
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestHealthReadsTheBodyNotTheStatus(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"data":{"status":"ok","database":"unavailable","system_database":"ok","data_database":"unavailable"}}`)
	})
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Healthy() {
		t.Errorf("Healthy() = true with an unavailable database: %+v", h)
	}
	c2, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"data":{"status":"ok","database":"ok","system_database":"ok","data_database":"ok"}}`)
	})
	if h, _ := c2.Health(context.Background()); !h.Healthy() {
		t.Errorf("Healthy() = false for an all-ok body: %+v", h)
	}
}

func TestDatapackRoundTrip(t *testing.T) {
	c, seen := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/datapacks":
			reply(w, 201, `{"data":{"datapack_id":"dp-1","agent_id":"meko_agent","name":"alpha","mcp_user_name":""}}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/datapacks":
			reply(w, 200, `{"data":[{"datapack_id":"dp-1","datapack_name":"alpha","created_by_agent_id":"meko_agent","created_at":"2026-10-02T10:00:00Z"}]}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/datapacks/dp-1":
			reply(w, 200, `{"data":{"datapack_id":"dp-1","datapack_name":"alpha","grant":"owner"}}`)
		case r.Method == "DELETE" && r.URL.Path == "/api/v1/datapacks/dp-1":
			reply(w, 200, `{"data":{"message":"datapack deleted"}}`)
		default:
			reply(w, 404, `{"data":null,"error":{"code":"Not Found","message":"nope"}}`)
		}
	})
	ctx := context.Background()
	dp, err := c.CreateDatapack(ctx, CreateDatapackRequest{Name: "alpha", Description: "d"})
	if err != nil || dp.ID != "dp-1" || dp.Name != "alpha" || dp.AgentID != "meko_agent" {
		t.Fatalf("CreateDatapack = %+v, %v", dp, err)
	}
	list, err := c.ListDatapacks(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "dp-1" || list[0].CreatedAt.IsZero() {
		t.Fatalf("ListDatapacks = %+v, %v", list, err)
	}
	got, err := c.GetDatapack(ctx, "dp-1")
	if err != nil || got.Grant != "owner" {
		t.Fatalf("GetDatapack = %+v, %v", got, err)
	}
	if err := c.DeleteDatapack(ctx, "dp-1"); err != nil {
		t.Fatalf("DeleteDatapack: %v", err)
	}
	if _, err := c.GetDatapack(ctx, "gone"); !IsNotFound(err) {
		t.Fatalf("GetDatapack(gone) err = %v, want a 404", err)
	}
	var create map[string]any
	if err := json.Unmarshal([]byte((*seen)[0].body), &create); err != nil {
		t.Fatal(err)
	}
	if create["name"] != "alpha" || create["description"] != "d" {
		t.Errorf("create body = %v", create)
	}
	if _, set := create["agent_id"]; set {
		t.Errorf("an empty agent_id was sent: %v", create)
	}
}

func TestErrorCarriesTheEnvelope(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 409, `{"data":null,"error":{"code":"CONFLICT","message":"agent exists"}}`)
	})
	_, err := c.CreateAgent(context.Background(), "a1", nil)
	var e *Error
	if !asError(err, &e) || e.Status != 409 || e.Code != "CONFLICT" || e.Message != "agent exists" {
		t.Fatalf("err = %#v", err)
	}
	if StatusOf(err) != 409 || IsNotFound(err) {
		t.Errorf("StatusOf/IsNotFound misread %v", err)
	}
}

func TestNonJSONErrorKeepsTheBody(t *testing.T) {
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream connect error", http.StatusBadGateway)
	})
	_, err := c.ListAgents(context.Background())
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream connect error") {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenLifecycleAndAuthHeader(t *testing.T) {
	c, seen := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			reply(w, 201, `{"data":{"token":"meko_tkn_abc","token_record":{"id":"t-1","name":"n","token_prefix":"meko_tkn_ab","scopes":["*"],"expires_at":"2026-12-01T00:00:00Z"}}}`)
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		default:
			reply(w, 200, `{"data":[{"id":"t-1","name":"n","scopes":["*"],"expires_at":"2026-12-01T00:00:00Z","revoked_at":"2026-10-02T10:00:00Z"}]}`)
		}
	})
	ctx := context.Background()
	tok, err := c.CreateToken(ctx, CreateTokenRequest{Name: "n", TTLDays: 1})
	if err != nil || tok.Value != "meko_tkn_abc" || tok.Record.ID != "t-1" {
		t.Fatalf("CreateToken = %+v, %v", tok, err)
	}
	authed := c.WithToken(tok.Value)
	if err := authed.RevokeToken(ctx, "t-1"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	recs, err := authed.ListTokens(ctx)
	if err != nil || len(recs) != 1 || recs[0].RevokedAt == nil {
		t.Fatalf("ListTokens = %+v, %v", recs, err)
	}
	if (*seen)[0].auth != "" {
		t.Errorf("an unauthenticated client sent %q", (*seen)[0].auth)
	}
	if (*seen)[1].auth != "Bearer meko_tkn_abc" || (*seen)[1].path != "/api/v1/me/meko-tokens/t-1" {
		t.Errorf("revoke request = %+v", (*seen)[1])
	}
}

func TestBasePathIsHonoured(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		reply(w, 200, `{"data":[]}`)
	}))
	defer srv.Close()
	c := New(srv.URL + "/ycp/")
	if _, err := c.ListAgents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/ycp/api/v1/agents" {
		t.Errorf("path = %q", path)
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestUploadKBFileSendsOneMultipartFileAndReadsTheDocumentStatus(t *testing.T) {
	c, seen := server(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upload") {
			reply(w, 202, `{"data":{"datapack_id":"dp1","document_key":"u/dp1/h/a.txt","filename":"a.txt","status":"uploaded"}}`)
			return
		}
		reply(w, 200, `{"data":{"document_name":"a.txt","document_status":"COMPLETED","chunks_processed":3}}`)
	})
	up, err := c.UploadKBFile(context.Background(), "dp1", "a.txt", []byte("hello kb"))
	if err != nil || up.DocumentKey != "u/dp1/h/a.txt" {
		t.Fatalf("upload = %+v, %v", up, err)
	}
	doc, err := c.KBDocumentStatus(context.Background(), "dp1", up.DocumentKey)
	if err != nil || doc.DocumentStatus != "COMPLETED" || *doc.ChunksProcessed != 3 {
		t.Fatalf("status = %+v, %v", doc, err)
	}
	first, second := (*seen)[0], (*seen)[1]
	if first.method != "POST" || first.path != "/api/v1/datapacks/dp1/knowledge-bases/upload" ||
		!strings.Contains(first.body, `name="file"; filename="a.txt"`) || !strings.Contains(first.body, "hello kb") {
		t.Errorf("upload request = %+v", first)
	}
	if second.method != "GET" || second.path != "/api/v1/datapacks/dp1/knowledge-bases/document-status" {
		t.Errorf("status request = %+v", second)
	}
}
