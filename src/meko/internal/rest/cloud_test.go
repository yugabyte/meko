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

	"github.com/labstack/echo/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

const (
	cloudKey     = "mko_tkn_cloud_0123456789wxyz"
	cloudDP      = "c0ffee00-0000-4000-8000-000000000001"
	missingCloud = "c0ffee00-0000-4000-8000-0000000000ff"
)

// fakeCloud is Meko Cloud as the routes call it: its REST API at /api/v1 (datapacks, create,
// delete, upload-url, document-status, artifacts, and sharing) and its MCP tools at /mcp
// (context_search, memory_add, which refuse arguments they don't know), all taking only cloudKey.
// calls records what it was asked.
type fakeCloud struct {
	URL   string
	mu    sync.Mutex
	calls []string
	args  map[string]map[string]any
	// twin, if set, is a datapack id Meko Cloud lists too, as "cloud-twin".
	twin string
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	f := &fakeCloud{args: map[string]map[string]any{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	for _, name := range []string{"context_search", "memory_add"} {
		known := map[string]bool{"conversation_id": true, "agent_id": true, "datapack_id": true, "query": true,
			"limit": true, "force_source": true, "text": true, "metadata": true}
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
			twin := ""
			if f.twin != "" {
				twin = `,{"datapack_id":"` + f.twin + `","datapack_name":"cloud-twin","grant":"owner"}`
			}
			reply(200, `{"data":[{"datapack_id":"`+cloudDP+`","datapack_name":"team-notes","grant":"owner",`+
				`"created_at":"2026-10-01T00:00:00Z","datapack_ysql_connection_string":"postgresql://x"}`+twin+`]}`)
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
		case "GET /api/v1/datapacks/" + cloudDP + "/artifacts":
			reply(200, `{"data":{"items":[{"filename":"deck.json","content_hash":"h1"}],"has_more":false,`+
				`"db_password":"hunter2"}}`)
		case "GET /api/v1/datapacks/incoming-shares", "GET /api/v1/datapacks/" + cloudDP + "/member-list":
			reply(200, `{"data":[]}`)
		case "POST /api/v1/datapacks/invite-action", "POST /api/v1/share/tok1/redeem",
			"POST /api/v1/datapacks/" + cloudDP + "/share":
			reply(200, `{"data":{"message":"ok"}}`)
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
// Cloud, with the link's key, for any credential; GET /datapacks lists both sides as one list, and
// the sharing routes go to Meko Cloud alone. No route takes or says a location: one sent is ignored.
func TestCloudDatapacks(t *testing.T) {
	cloud := newFakeCloud(t)
	dir := t.TempDir()
	link := &cloudlink.Service{
		Store:    cloudlink.Store{File: filepath.Join(dir, "cloud.toml"), Secrets: secrets.NewFiles(dir)},
		Check:    func(context.Context, string, string) error { return nil },
		CheckAPI: func(context.Context, string) error { return nil },
	}
	as := "" // a credential to answer as instead of the request's: "local" is a browser on local access
	e := testenv.Start(t, testenv.Options{Cloud: link, Credential: func(c echo.Context) string {
		if as != "" {
			return as
		}
		return server.Credential(c)
	}})
	admin := func(method, path string, body any) reply {
		return doAs(t, e, testenv.AdminToken, method, path, body)
	}

	// Unlinked: this Meko's datapacks alone, each opened by its page here, and a datapack this Meko
	// doesn't have is a 404 as before. A location is ignored.
	all := arr(okData(t, e, "GET", "/datapacks?location=cloud", nil, 200))
	if len(all) != 1 || obj(all[0])["datapack_id"] != e.DatapackID ||
		obj(all[0])["web_url"] != "/datapacks/"+e.DatapackID || strings.Contains(toJSON(all), "location") {
		t.Fatalf("the listing, unlinked: %v", all)
	}
	if r := admin("DELETE", "/datapacks/"+cloudDP+"?location=cloud", nil); r.Status != 404 || r.Error.Code != "NOT_FOUND" {
		t.Fatalf("delete unlinked: %+v", r)
	}
	// Syncing a datapack to Meko Cloud is the owner's, and needs the link.
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/cloud-sync/precheck", nil, 403, "FORBIDDEN")
	if r := admin("POST", "/datapacks/"+e.DatapackID+"/cloud-sync", nil); r.Status != 409 ||
		r.Error.Code != "CLOUD_NOT_LINKED" {
		t.Fatalf("a sync, unlinked: %+v", r)
	}
	// Sharing is Meko Cloud's: unlinked, it says so.
	wantErr(t, e, "GET", "/datapacks/incoming-shares", nil, 409, "CLOUD_NOT_LINKED")
	wantErr(t, e, "GET", "/datapacks/"+e.DatapackID+"/member-list", nil, 409, "CLOUD_NOT_LINKED")
	made := obj(okData(t, e, "POST", "/datapacks", map[string]any{"name": "made-here", "location": "cloud"}, 201))
	if id, _ := made["datapack_id"].(string); !dpExists(e, id) || made["location"] != nil ||
		made["web_url"] != "/datapacks/"+id {
		t.Fatalf("a create, unlinked, is this Meko's: %v", made)
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

	// Listing takes any credential, copies only the fields Meko shows, and sorts both sides by name,
	// one row per id: Meko Cloud's copy of this Meko's datapack is left out. Meko Cloud's rows open in
	// its web app, unknown for this link.
	cloud.twin = e.DatapackID
	here, _ := e.Service.Datapack(context.Background(), e.DatapackID)
	all = arr(okData(t, e, "GET", "/datapacks?location=local", nil, 200))
	byID := map[string]map[string]any{}
	for i, row := range all {
		byID[str(obj(row)["datapack_id"])] = obj(row)
		if i > 0 && strings.ToLower(str(obj(all[i-1])["datapack_name"])) > strings.ToLower(str(obj(row)["datapack_name"])) {
			t.Errorf("not sorted by name: %v", all)
		}
	}
	if len(all) != 3 || byID[e.DatapackID]["datapack_name"] != here.Name ||
		byID[e.DatapackID]["web_url"] != "/datapacks/"+e.DatapackID || byID[cloudDP]["datapack_name"] != "team-notes" ||
		byID[cloudDP]["web_url"] != nil || strings.Contains(toJSON(all), "postgresql") ||
		strings.Contains(toJSON(all), "location") {
		t.Fatalf("the listing, linked: %v", all)
	}
	cloud.twin = ""

	// A web UI tab from before (a browser sending location=local) gets this Meko's datapacks alone,
	// and its search stays here; a tab of this UI, which names no location, gets both.
	as = "local"
	if rows := arr(okData(t, e, "GET", "/datapacks?location=local", nil, 200)); len(rows) != 2 ||
		strings.Contains(toJSON(rows), cloudDP) {
		t.Fatalf("a stale tab's listing: %v", rows)
	}
	if rows := arr(okData(t, e, "GET", "/datapacks", nil, 200)); len(rows) != 3 {
		t.Fatalf("this UI's listing: %v", rows)
	}
	wantErr(t, e, "POST", "/datapacks/"+cloudDP+"/search?location=local", map[string]any{"query": "x"}, 404,
		"NOT_FOUND")
	as = ""

	// Linked, a create is Meko Cloud's, whatever location it names, so the client token can't make one.
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "x"}, 403, "FORBIDDEN")
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "kept-here", "location": "local"}, 403, "FORBIDDEN")
	wantErr(t, e, "GET", "/cloud/datapacks", nil, 404, "NOT_FOUND")
	// A Meko Cloud without the import routes takes no synced datapack, and nothing starts.
	if r := admin("POST", "/datapacks/"+e.DatapackID+"/cloud-sync/precheck", nil); r.Status != 409 ||
		r.Error.Code != "CLOUD_SYNC_UNSUPPORTED" {
		t.Fatalf("a precheck against a Meko Cloud without imports: %+v", r)
	}
	wantErr(t, e, "GET", "/datapacks/"+e.DatapackID+"/cloud-sync", nil, 404, "NOT_FOUND")
	// A project folder here files only this Meko's datapacks, and a new one is Meko Cloud's.
	if r := admin("POST", "/datapacks", map[string]any{"name": "filed", "folder_id": "f"}); r.Status != 400 ||
		r.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("a create in a folder, linked: %+v", r)
	}

	// A client token's id this Meko doesn't have goes to Meko Cloud, as on /mcp: the same path and
	// query with the key, and Meko Cloud's reply without its credentials.
	arts := obj(okData(t, e, "GET", "/datapacks/"+cloudDP+"/artifacts?q=deck&page=1", nil, 200))
	if len(arr(arts["items"])) != 1 || strings.Contains(toJSON(arts), "hunter2") ||
		!strings.HasPrefix(cloud.last(), "GET /api/v1/datapacks/"+cloudDP+"/artifacts?q=deck&page=1 ") {
		t.Fatalf("artifacts in Cloud: %v (Cloud got %q)", arts, cloud.last())
	}
	if st := obj(okData(t, e, "GET", "/datapacks/"+cloudDP+"/knowledge-bases/document-status?document_key=k1",
		nil, 200)); st["state"] != "completed" {
		t.Fatalf("document-status in Cloud for a client token: %v", st)
	}
	wantErr(t, e, "POST", "/datapacks/"+missingCloud+"/memories", map[string]any{"text": "x"}, 404, "NOT_FOUND")
	wantErr(t, e, "GET", "/datapacks/"+missingCloud+"/artifacts", nil, 404, "NOT_FOUND")
	// Meko's own routes answer for this Meko's datapacks alone.
	wantErr(t, e, "GET", "/datapacks/"+cloudDP+"/usage", nil, 404, "NOT_FOUND")

	// Sharing goes to Meko Cloud; this Meko's datapack can't be shared until it's there.
	for _, call := range [][2]string{{"GET", "/datapacks/incoming-shares"}, {"POST", "/datapacks/invite-action"},
		{"POST", "/share/tok1/redeem"}, {"POST", "/datapacks/" + cloudDP + "/share"},
		{"GET", "/datapacks/" + cloudDP + "/member-list"}} {
		if r := do(t, e, call[0], call[1], map[string]any{}); r.Status != 200 ||
			!strings.HasPrefix(cloud.last(), call[0]+" /api/v1"+call[1]+" ") {
			t.Fatalf("%s %s: %+v (Cloud got %q)", call[0], call[1], r, cloud.last())
		}
	}
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/share", map[string]any{"recipient_email": "bob@example.com"},
		409, "DATAPACK_LOCAL")
	wantErr(t, e, "GET", "/datapacks/"+e.DatapackID+"/share-link", nil, 409, "DATAPACK_LOCAL")

	// Only the routes listed reach Meko Cloud: a datapack that isn't a UUID, or a path parameter that
	// could name another path there, is this Meko's 400.
	sent := cloud.last()
	for _, call := range [][2]string{{"GET", "/datapacks/not-a-uuid/member-list"},
		{"GET", "/datapacks/../member-list"}, {"POST", "/datapacks/" + cloudDP + "/invite/../resend"},
		{"POST", "/datapacks/" + cloudDP + "/invite/..%2F..%2Fadmin/resend"}, {"POST", "/share/a%2Fb/redeem"},
		{"GET", "/datapacks/" + cloudDP + "/artifacts/.."}} {
		wantErr(t, e, call[0], call[1], nil, 400, "VALIDATION_ERROR")
	}
	// A browser opens Meko Cloud's datapacks in its web app, never on this Meko's pages: its :id
	// routes stay here.
	as = "local"
	wantErr(t, e, "GET", "/datapacks/"+cloudDP, nil, 404, "NOT_FOUND")
	wantErr(t, e, "GET", "/datapacks/"+cloudDP+"/artifacts", nil, 404, "NOT_FOUND")
	as = ""
	if cloud.last() != sent {
		t.Fatalf("Meko Cloud got %q", cloud.last())
	}

	r := admin("POST", "/datapacks", map[string]any{"name": "team-notes", "location": "local"})
	if r.Status != 201 || obj(r.Data)["datapack_id"] != cloudDP || strings.Contains(toJSON(r.Data), "secret") ||
		obj(r.Data)["web_url"] != nil || !strings.Contains(toJSON(r.Data), `"web_url":null`) ||
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
		!strings.Contains(cloud.last(), "document_key=k%201") {
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
	// An id this Meko has is served here, even when Meko Cloud has it too (an export keeps ids), and
	// whatever location the request names.
	r = admin("POST", "/datapacks/"+e.DatapackID+"/search?location=cloud",
		map[string]any{"query": "the nightly build runs at two", "force_source": "memory"})
	if r.Status != 200 || !strings.Contains(toJSON(r.Data), "the nightly build runs at two") ||
		cloud.args["context_search"]["datapack_id"] == e.DatapackID {
		t.Fatalf("search of this Meko's datapack while linked: %+v", r)
	}
	// A datapack neither side has is a 404 from search and memories too.
	if r = admin("POST", "/datapacks/"+missingCloud+"/search", map[string]any{"query": "x"}); r.Status != 404 ||
		r.Error.Code != "NOT_FOUND" {
		t.Fatalf("search of a datapack Cloud hasn't: %+v", r)
	}
	if r = admin("DELETE", "/datapacks/"+cloudDP+"?location=local", nil); r.Status != 200 {
		t.Fatalf("delete in Cloud: %+v", r)
	}

	// Meko Cloud's own 4xx keeps its status and code; a refused key is a 502, never a 401.
	if r = admin("DELETE", "/datapacks/"+missingCloud, nil); r.Status != 404 || r.Error.Code != "NOT_FOUND" {
		t.Fatalf("a datapack Cloud hasn't: %+v", r)
	}
	if _, err := link.Link(context.Background(), cloudlink.Update{Key: ptr("mko_tkn_stale_0123456789abcd")}); err != nil {
		t.Fatal(err)
	}
	// The listing fails with Meko Cloud: there's no whole list without it.
	wantErr(t, e, "GET", "/datapacks", nil, 502, "CLOUD_KEY_REFUSED")
	if r = admin("POST", "/datapacks/"+cloudDP+"/search", map[string]any{"query": "x"}); r.Status != 502 ||
		r.Error.Code != "CLOUD_KEY_REFUSED" || strings.Contains(r.Error.Message, "mko_tkn") {
		t.Fatalf("search with a refused key: %+v", r)
	}
	wantErr(t, e, "GET", "/datapacks/"+cloudDP+"/artifacts", nil, 502, "CLOUD_KEY_REFUSED")
}

func ptr(s string) *string { return &s }

// dpExists says whether this Meko has the datapack.
func dpExists(e *testenv.Env, id string) bool {
	_, err := e.Service.Datapack(context.Background(), id)
	return err == nil
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
