package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testCloudKey = "mko_tkn_cloud_0123456789wxyz"

// fakeCloud is a Meko server for the link's checks: MCP at /mcp, taking only testCloudKey, and the
// REST API's envelope at /api/v1.
func fakeCloud(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
