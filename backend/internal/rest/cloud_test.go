package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/secrets"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

const (
	cloudKey     = "mko_tkn_cloud_0123456789wxyz"
	cloudDP      = "c0ffee00-0000-4000-8000-000000000001"
	missingCloud = "c0ffee00-0000-4000-8000-0000000000ff"
)

// fakeCloud is Meko Cloud as the routes call it: its REST API at /api/v1 (datapacks, create,
// delete, upload-url, document-status) and its MCP tools at /mcp (context_search, memory_add, which
// refuse arguments they don't know), all taking only cloudKey. calls records what it was asked.
type fakeCloud struct {
	URL   string
	mu    sync.Mutex
	calls []string
	args  map[string]map[string]any
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	f := &fakeCloud{args: map[string]map[string]any{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	for _, name := range []string{"context_search", "memory_add"} {
		known := map[string]bool{"conversation_id": true, "agent_id": true, "datapack_id": true, "query": true,
			"limit": true, "force_source": true, "text": true}
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var a map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &a)
				f.mu.Lock()
				f.args[name] = a
				f.mu.Unlock()
				for k := range a {
					if !known[k] {
						return &mcp.CallToolResult{IsError: true,
							Content: []mcp.Content{&mcp.TextContent{Text: "Unexpected keyword argument " + k}}}, nil
					}
				}
				text := `{"results":[{"id":"m-1","memory":"kept in Cloud"}]}`
				if name == "context_search" {
					text = `{"results":{"memory":[{"memory":"kept in Cloud"}],"knowledge_base":null,"conversation":null}}`
				}
				if a["datapack_id"] != cloudDP {
					text = `{"error":"datapack_not_found","detail":"No such datapack."}`
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
			})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cloudKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"data":null,"error":{"code":"UNAUTHORIZED","message":"bad key"}}`))
			return
		}
		if r.URL.Path == "/mcp" {
			h.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		f.mu.Unlock()
		reply := func(status int, v string) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(v))
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/datapacks":
			reply(200, `{"data":[{"datapack_id":"`+cloudDP+`","datapack_name":"team-notes","grant":"owner",`+
				`"created_at":"2026-10-01T00:00:00Z","datapack_ysql_connection_string":"postgresql://x"}]}`)
		case "POST /api/v1/datapacks":
			reply(201, `{"data":{"datapack_id":"`+cloudDP+`","name":"team-notes","agent_id":"meko_agent",`+
				`"mcp_server":{"url":"","api_key":"secret"}}}`)
		case "DELETE /api/v1/datapacks/" + cloudDP:
			reply(200, `{"data":{"message":"datapack deleted"}}`)
		case "POST /api/v1/datapacks/" + cloudDP + "/knowledge-bases/upload-url":
			reply(200, `{"data":{"url":"https://s3.example.com/up","fields":{"key":"k1"},"key":"k1",`+
				`"already_uploaded":false}}`)
		case "GET /api/v1/datapacks/" + cloudDP + "/knowledge-bases/document-status":
			reply(200, `{"data":{"document_key":"k1","state":"completed","message":"Indexed."}}`)
		default:
			reply(404, `{"data":null,"error":{"code":"NOT_FOUND","message":"datapack not found"}}`)
		}
	}))
	t.Cleanup(ts.Close)
	f.URL = ts.URL
	return f
}

func (f *fakeCloud) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

// doAs is do with another token.
func doAs(t *testing.T, e *testenv.Env, token, method, path string, body any) reply {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.URL+"/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out reply
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: not an envelope: %s", method, path, raw)
	}
	out.Status = resp.StatusCode
	return out
}

// The routes that take a datapack serve this Meko's and send one it doesn't have to the linked Meko
// Cloud, with the link's key, for the owner or the admin token only; GET /datapacks?location= lists
// Meko Cloud's for any credential.
func TestCloudDatapacks(t *testing.T) {
	cloud := newFakeCloud(t)
	dir := t.TempDir()
	link := &cloudlink.Service{
		Store:    cloudlink.Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)},
		Check:    func(context.Context, string, string) error { return nil },
		CheckAPI: func(context.Context, string) error { return nil },
	}
	e := testenv.Start(t, testenv.Options{Cloud: link})
	admin := func(method, path string, body any) reply {
		return doAs(t, e, testenv.AdminToken, method, path, body)
	}

	// Unlinked: no Cloud listing, and a datapack this Meko doesn't have is a 404 as before.
	wantErr(t, e, "GET", "/datapacks?location=cloud", nil, 409, "CLOUD_NOT_LINKED")
	wantErr(t, e, "GET", "/datapacks?location=elsewhere", nil, 400, "VALIDATION_ERROR")
	all := arr(okData(t, e, "GET", "/datapacks", nil, 200))
	if len(all) != 1 || obj(all[0])["location"] != "local" || obj(all[0])["datapack_id"] != e.DatapackID {
		t.Fatalf("the default listing (all), unlinked: %v", all)
	}
	if r := admin("DELETE", "/datapacks/"+cloudDP, nil); r.Status != 404 || r.Error.Code != "NOT_FOUND" {
		t.Fatalf("delete unlinked: %+v", r)
	}
	if r := admin("POST", "/datapacks", map[string]any{"name": "x", "location": "cloud"}); r.Status != 409 {
		t.Fatalf("create in Cloud unlinked: %+v", r)
	}
	// Unlinked is a 409 whatever the credential.
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "x", "location": "cloud"}, 409, "CLOUD_NOT_LINKED")
	wantErr(t, e, "DELETE", "/datapacks/"+cloudDP+"?location=cloud", nil, 409, "CLOUD_NOT_LINKED")
	wantErr(t, e, "GET", "/cloud/datapacks", nil, 409, "CLOUD_NOT_LINKED")
	made := obj(okData(t, e, "POST", "/datapacks", map[string]any{"name": "made-here"}, 201))
	if id, _ := made["datapack_id"].(string); !dpExists(e, id) {
		t.Fatalf("a create with no location, unlinked, is this Meko's: %v", made)
	}

	// This Meko's datapack: search and memories answer what the tools do.
	added := obj(okData(t, e, "POST", "/datapacks/"+e.DatapackID+"/memories",
		map[string]any{"text": "the nightly build runs at two", "agent_id": "rest_test"}, 200))
	if len(arr(added["results"])) != 1 {
		t.Fatalf("memories: %v", added)
	}
	found := obj(okData(t, e, "POST", "/datapacks/"+e.DatapackID+"/search",
		map[string]any{"query": "the nightly build runs at two", "force_source": "memory"}, 200))
	if !strings.Contains(toJSON(found), "the nightly build runs at two") {
		t.Fatalf("search: %v", found)
	}
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/search", map[string]any{"query": ""}, 400, "VALIDATION_ERROR")
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/search",
		map[string]any{"query": "x", "force_source": "everything"}, 400, "invalid_force_source")
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/memories", map[string]any{"text": " "}, 400, "VALIDATION_ERROR")

	if _, err := link.Link(context.Background(), cloudlink.Update{URL: ptr(cloud.URL + "/mcp"),
		APIURL: ptr(cloud.URL + "/api/v1"), Key: ptr(cloudKey)}); err != nil {
		t.Fatal(err)
	}

	// Listing takes any credential, and copies only the fields Meko shows.
	rows := arr(okData(t, e, "GET", "/datapacks?location=cloud", nil, 200))
	if len(rows) != 1 || obj(rows[0])["location"] != "cloud" || obj(rows[0])["datapack_name"] != "team-notes" ||
		strings.Contains(toJSON(rows), "postgresql") {
		t.Fatalf("location=cloud: %v", rows)
	}
	// This Meko's two (the default and made-here), then Meko Cloud's one.
	if all = arr(okData(t, e, "GET", "/datapacks?location=all", nil, 200)); len(all) != 3 ||
		obj(all[1])["location"] != "local" || obj(all[2])["location"] != "cloud" {
		t.Fatalf("location=all: %v", all)
	}
	if all = arr(okData(t, e, "GET", "/datapacks", nil, 200)); len(all) != 3 || obj(all[2])["location"] != "cloud" {
		t.Fatalf("the default listing is both: %v", all)
	}
	if local := arr(okData(t, e, "GET", "/datapacks?location=local", nil, 200)); len(local) != 2 ||
		obj(local[0])["location"] != nil {
		t.Fatalf("location=local is this Meko's, as before: %v", local)
	}

	// Acting on Meko Cloud takes the admin token (or the owner on the web listener).
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "x", "location": "cloud"}, 403, "FORBIDDEN")
	// Linked, a create with no location is Meko Cloud's, so the client token can't make one.
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "x"}, 403, "FORBIDDEN")
	// /api/v1/cloud passes a client token's request on to Meko Cloud's REST API, with the key.
	rows = arr(okData(t, e, "GET", "/cloud/datapacks?q=team", nil, 200))
	if len(rows) != 1 || obj(rows[0])["datapack_name"] != "team-notes" ||
		!strings.HasPrefix(cloud.last(), "GET /api/v1/datapacks?q=team") {
		t.Fatalf("/cloud/datapacks: %v (Cloud got %q)", rows, cloud.last())
	}

	// Linked, no datapack is made here, whatever the credential.
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "kept-here", "location": "local"}, 409, "CLOUD_LINKED")
	if r := admin("POST", "/datapacks", map[string]any{"name": "kept-here", "location": "local"}); r.Status != 409 ||
		r.Error.Code != "CLOUD_LINKED" {
		t.Fatalf("location local while linked, admin: %+v", r)
	}
	wantErr(t, e, "DELETE", "/datapacks/"+cloudDP+"?location=cloud", nil, 403, "FORBIDDEN")
	wantErr(t, e, "POST", "/datapacks/"+cloudDP+"/search?location=cloud", map[string]any{"query": "x"}, 403,
		"FORBIDDEN")
	// A client token's id this Meko doesn't have is this Meko's 404, linked or not.
	wantErr(t, e, "DELETE", "/datapacks/"+cloudDP, nil, 404, "NOT_FOUND")
	wantErr(t, e, "POST", "/datapacks/"+missingCloud+"/memories", map[string]any{"text": "x"}, 404, "NOT_FOUND")
	wantErr(t, e, "GET", "/datapacks/"+cloudDP+"/knowledge-bases/document-status?document_key=k1", nil, 404,
		"NOT_FOUND")

	r := admin("POST", "/datapacks", map[string]any{"name": "team-notes"})
	if r.Status != 201 || obj(r.Data)["datapack_id"] != cloudDP || strings.Contains(toJSON(r.Data), "secret") ||
		!strings.HasPrefix(cloud.last(), `POST /api/v1/datapacks {"name":"team-notes"}`) {
		t.Fatalf("create in Cloud: %+v (Cloud got %q)", r, cloud.last())
	}
	r = admin("POST", "/datapacks/"+cloudDP+"/knowledge-bases/upload-url",
		map[string]any{"filename": "a.md", "size": 4, "content_hash": strings.Repeat("a", 64), "client": "meko-cli"})
	if r.Status != 200 || obj(r.Data)["url"] != "https://s3.example.com/up" ||
		!strings.Contains(cloud.last(), `"client":"meko-cli"`) {
		t.Fatalf("upload-url in Cloud: %+v (Cloud got %q)", r, cloud.last())
	}
	r = admin("GET", "/datapacks/"+cloudDP+"/knowledge-bases/document-status?document_key=k%201", nil)
	if r.Status != 200 || obj(r.Data)["state"] != "completed" ||
		!strings.Contains(cloud.last(), "document_key=k+1") {
		t.Fatalf("document-status in Cloud: %+v (Cloud got %q)", r, cloud.last())
	}
	r = admin("POST", "/datapacks/"+cloudDP+"/search", map[string]any{"query": "x", "limit": 3})
	if r.Status != 200 || !strings.Contains(toJSON(r.Data), "kept in Cloud") {
		t.Fatalf("search in Cloud: %+v", r)
	}
	if a := cloud.args["context_search"]; a["datapack_id"] != cloudDP || a["conversation_id"] == "" ||
		a["limit"] != float64(3) {
		t.Fatalf("context_search's arguments: %v", a)
	}
	r = admin("POST", "/datapacks/"+cloudDP+"/memories", map[string]any{"text": "kept in Cloud", "agent_id": "a"})
	if r.Status != 200 || !strings.Contains(toJSON(r.Data), "m-1") || cloud.args["memory_add"]["agent_id"] != "a" {
		t.Fatalf("memories in Cloud: %+v %v", r, cloud.args["memory_add"])
	}
	// location says which side, even for an id this Meko has (an export keeps ids).
	if r = admin("DELETE", "/datapacks/"+e.DatapackID+"?location=cloud", nil); r.Status != 404 ||
		!dpExists(e, e.DatapackID) || !strings.HasPrefix(cloud.last(), "DELETE /api/v1/datapacks/"+e.DatapackID+" ") {
		t.Fatalf("location=cloud for an id this Meko has: %+v (Cloud got %q)", r, cloud.last())
	}
	if r = admin("DELETE", "/datapacks/"+cloudDP+"?location=local", nil); r.Status != 404 {
		t.Fatalf("location=local for Cloud's id: %+v", r)
	}
	wantErr(t, e, "DELETE", "/datapacks/"+cloudDP+"?location=elsewhere", nil, 400, "VALIDATION_ERROR")
	// A datapack neither side has is a 404 from search and memories too.
	if r = admin("POST", "/datapacks/"+missingCloud+"/search", map[string]any{"query": "x"}); r.Status != 404 ||
		r.Error.Code != "NOT_FOUND" {
		t.Fatalf("search of a datapack Cloud hasn't: %+v", r)
	}
	if r = admin("DELETE", "/datapacks/"+cloudDP, nil); r.Status != 200 {
		t.Fatalf("delete in Cloud: %+v", r)
	}

	// Meko Cloud's own 4xx keeps its status and code; a refused key is a 502, never a 401.
	if r = admin("DELETE", "/datapacks/"+missingCloud, nil); r.Status != 404 || r.Error.Code != "NOT_FOUND" {
		t.Fatalf("a datapack Cloud hasn't: %+v", r)
	}
	if _, err := link.Link(context.Background(), cloudlink.Update{Key: ptr("mko_tkn_stale_0123456789abcd")}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e, "GET", "/datapacks?location=cloud", nil, 502, "CLOUD_KEY_REFUSED")
	if r = admin("POST", "/datapacks/"+cloudDP+"/search", map[string]any{"query": "x"}); r.Status != 502 ||
		r.Error.Code != "CLOUD_KEY_REFUSED" || strings.Contains(r.Error.Message, "mko_tkn") {
		t.Fatalf("search with a refused key: %+v", r)
	}
}

func ptr(s string) *string { return &s }

// dpExists says whether this Meko has the datapack.
func dpExists(e *testenv.Env, id string) bool {
	_, err := e.Service.Datapack(context.Background(), id)
	return err == nil
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
