package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testCloudKey = "mko_tkn_cloud_0123456789wxyz"

// fakeCloudDown makes fakeCloud's GET /api/v1/datapacks fail with a 500.
var fakeCloudDown atomic.Bool

// fakeCloud is a Meko server for the link's checks: MCP at /mcp, taking only testCloudKey, and the
// REST API's envelope at /api/v1, where GET /datapacks gives two rows to testCloudKey.
func fakeCloud(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/datapacks" {
			switch {
			case r.Header.Get("Authorization") != "Bearer "+testCloudKey:
				http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"no"}}`, http.StatusUnauthorized)
			case fakeCloudDown.Load():
				http.Error(w, `{"data":null,"error":{"code":"INTERNAL","message":"Cloud is down."}}`,
					http.StatusInternalServerError)
			default:
				_, _ = w.Write([]byte(`{"data":[{"datapack_id":"c0ffee00-0000-4000-8000-000000000001",` +
					`"datapack_name":"team-runbooks","grant":"viewer","created_at":"2026-10-02T00:00:00Z",` +
					`"datapack_ysql_connection_string":"postgresql://db.example.com/dp"},` +
					`{"datapack_id":"c0ffee00-0000-4000-8000-000000000002","datapack_name":"cloud-notes",` +
					`"description":"mine","grant":"owner","created_at":"2026-10-01T00:00:00Z"}]}`))
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testCloudKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

type tokenJSON struct {
	Cloud cloudToken `json:"cloud"`
}

func readCloudToken(t *testing.T, dir string) cloudToken {
	t.Helper()
	code, out, errs := run("token", "--data-dir", dir, "--json")
	want(t, "token --json", code, 0, out+errs, `"cloud":`)
	var tj tokenJSON
	if err := json.Unmarshal([]byte(out), &tj); err != nil {
		t.Fatal(err)
	}
	return tj.Cloud
}

// meko cloud_link saves the link itself while no server runs, and through the server while one does;
// extensions read it from token --json, and status names it.
func TestCloudLink(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)

	withStdin(t, "wrong_key_0123456789\n")
	code, _, errs := run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "a key the server refuses", code, 1, errs, "didn't accept that key")
	if c := readCloudToken(t, dir); c.Set {
		t.Fatalf("a refused link was saved: %+v", c)
	}

	withStdin(t, testCloudKey+"\n")
	code, out, errs := run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link with no server", code, 0, out+errs, "Linked to "+cloud+"/mcp (key …wxyz)")
	c := readCloudToken(t, dir)
	wantCloud := cloudToken{Set: true, URL: cloud + "/mcp", APIKey: testCloudKey, WebURL: cloud, APIURL: cloud + "/api/v1"}
	if c != wantCloud {
		t.Fatalf("token --json cloud = %+v, want %+v", c, wantCloud)
	}
	code, out, _ = run("status", "--data-dir", dir)
	want(t, "status", code, exitNotRunning, out, "Meko Cloud:    linked to "+cloud+"/mcp (key …wxyz)")

	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--off")
	want(t, "cloud_link --off through the server", code, 0, out+errs, "Meko Cloud is unlinked")
	if c := readCloudToken(t, dir); c != (cloudToken{Set: true}) {
		t.Fatalf("unlinked: %+v", c)
	}
	// Linking again with only the key keeps the URLs saved before.
	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir)
	want(t, "cloud_link through the server", code, 0, out+errs, "Linked to "+cloud+"/mcp")

	code, _, errs = run("cloud_link", "--data-dir", dir, "--off", "--url", "x")
	want(t, "--off with a URL", code, exitUsage, errs, "--off takes no URLs")

	// A link that can't be read says so, and the rest of token --json still answers.
	if err := os.WriteFile(filepath.Join(dir, "cloud.toml"), []byte("linked = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := readCloudToken(t, dir); c.Error == "" || c.Set || c.APIKey != "" {
		t.Fatalf("an unreadable link: %+v", c)
	}
}

// meko datapacks --cloud lists this Meko's datapacks, then the linked Meko Cloud's, through the
// running server; unlinked it says so, and a Cloud that fails still leaves the local rows printed.
func TestDatapacksCloud(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")

	code, out, errs = run("datapacks", "--data-dir", dir, "--cloud")
	want(t, "--cloud unlinked", code, 0, out, "  default  local  (Created by Meko on first start.)")
	want(t, "--cloud unlinked", code, 0, errs, "Meko Cloud isn't linked: meko cloud_link links it")

	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")

	code, out, errs = run("datapacks", "--data-dir", dir, "--cloud")
	want(t, "--cloud linked", code, 0, out+errs,
		"c0ffee00-0000-4000-8000-000000000001  team-runbooks  cloud shared\n"+
			"c0ffee00-0000-4000-8000-000000000002  cloud-notes  cloud  (mine)\n")
	if strings.Index(out, "  default  local") > strings.Index(out, "team-runbooks") ||
		strings.Contains(out, "postgresql") {
		t.Fatalf("local rows come first, and nothing else of Cloud's rows:\n%s", out)
	}
	code, out, _ = run("datapacks", "--data-dir", dir)
	if code != 0 || strings.Contains(out, "team-runbooks") || strings.Contains(out, "local") {
		t.Fatalf("without --cloud it's unchanged: %d\n%s", code, out)
	}

	code, out, errs = run("datapacks", "--data-dir", dir, "--cloud", "--json")
	var j struct {
		Local []map[string]any `json:"local"`
		Cloud struct {
			Linked    bool             `json:"linked"`
			Datapacks []map[string]any `json:"datapacks"`
		} `json:"cloud"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &j) != nil || len(j.Local) == 0 || !j.Cloud.Linked ||
		len(j.Cloud.Datapacks) != 2 ||
		j.Cloud.Datapacks[0]["web_url"] != cloud+"/datapacks/c0ffee00-0000-4000-8000-000000000001" {
		t.Fatalf("--cloud --json: %d %s %s", code, out, errs)
	}

	fakeCloudDown.Store(true)
	t.Cleanup(func() { fakeCloudDown.Store(false) })
	code, out, errs = run("datapacks", "--data-dir", dir, "--cloud")
	want(t, "--cloud with Cloud down", code, 1, errs,
		"couldn't list Meko Cloud's datapacks: Meko Cloud couldn't list datapacks: Cloud is down")
	want(t, "--cloud with Cloud down", code, 1, out, "  default  local")
}
