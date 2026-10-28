package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
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

// mekosh datapacks lists this Meko's datapacks, then, while Meko Cloud is linked, its datapacks,
// through the running server; --local leaves Meko Cloud out, and a Cloud that fails leaves a
// warning after the local rows.
func TestDatapacksCloud(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")

	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "unlinked", code, 0, out, "  default  (Created by Meko on first start.)")
	if errs != "" || strings.Contains(out, "local") {
		t.Fatalf("unlinked, the list is unmarked and says nothing of Meko Cloud:\n%s%s", out, errs)
	}
	code, out, _ = runSh("datapacks", "--data-dir", dir, "--json")
	want(t, "--json unlinked", code, 0, out, `"cloud":{"datapacks":[],"linked":false}`)

	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")

	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "linked", code, 0, out+errs,
		"c0ffee00-0000-4000-8000-000000000001  team-runbooks  cloud shared\n"+
			"c0ffee00-0000-4000-8000-000000000002  cloud-notes  cloud  (mine)\n")
	if strings.Index(out, "  default  local") > strings.Index(out, "team-runbooks") ||
		strings.Contains(out, "postgresql") {
		t.Fatalf("local rows come first, and nothing else of Cloud's rows:\n%s", out)
	}
	code, out, _ = runSh("datapacks", "--data-dir", dir, "--local")
	if code != 0 || strings.Contains(out, "team-runbooks") || strings.Contains(out, "local") {
		t.Fatalf("--local leaves Meko Cloud out: %d\n%s", code, out)
	}
	code, out, _ = runSh("datapacks", "--data-dir", dir, "--local", "--json")
	if code != 0 || !strings.HasPrefix(out, "[") {
		t.Fatalf("--local --json is the tool's reply: %d\n%s", code, out)
	}

	code, out, errs = runSh("datapacks", "--data-dir", dir, "--json")
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
		t.Fatalf("--json: %d %s %s", code, out, errs)
	}

	fakeCloudDown.Store(true)
	t.Cleanup(func() { fakeCloudDown.Store(false) })
	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "Cloud down", code, 0, errs,
		"mekosh: couldn't list Meko Cloud's datapacks: Meko Cloud couldn't list datapacks: Cloud is down "+
			"(mekosh datapacks --local skips Meko Cloud)")
	want(t, "Cloud down", code, 0, out, "  default  local")
}

// mekosh works on the linked Meko Cloud's datapacks. The "Meko Cloud" here is a second Meko server,
// which serves the REST routes and MCP tools mekosh calls with Cloud's shapes: a name only it has
// goes there, cloud: picks it when both have one, and every message about its datapacks says so.
func TestCloudDatapacks(t *testing.T) {
	withMemoryEngine(t)
	t.Setenv("MEKO_SECRETS", "file")
	dir, port := setup(t)
	startReady(t, dir, port)
	cloudDir, cloudPort := shortDir(t), strconv.Itoa(freePort(t))
	t.Setenv("MEKO_WEB_PORT", strconv.Itoa(freePort(t)))
	t.Cleanup(func() { run("stop", "--data-dir", cloudDir, "--keep-extensions") })
	startReady(t, cloudDir, cloudPort)

	// Unlinked: cloud: says so, and a missing name is only missing here.
	code, _, errs := runSh("add_memory", "--data-dir", dir, "x", "--datapack", "cloud:team-notes")
	want(t, "cloud: unlinked", code, 1, errs, "mekosh: this Meko isn't linked to Meko Cloud: meko cloud_link links it\n")
	code, _, errs = runSh("search", "--data-dir", dir, "x", "--datapack", "nope")
	want(t, "a missing name unlinked", code, 1, errs, `no datapack is named "nope" (mekosh datapacks lists them)`)

	_, key, _ := run("token", "--data-dir", cloudDir)
	withStdin(t, key)
	cloud := "http://127.0.0.1:" + cloudPort
	code, out, errs := run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--api-url", cloud+"/api/v1")
	want(t, "cloud_link", code, 0, out+errs, "Linked to "+cloud+"/mcp")

	code, out, errs = runSh("create_datapack", "--data-dir", dir, "team-notes", "--cloud")
	want(t, "create_datapack --cloud", code, 0, out+errs, "Created Meko Cloud datapack team-notes (")
	if _, out, _ = runSh("datapacks", "--data-dir", dir, "--local"); strings.Contains(out, "team-notes") {
		t.Fatalf("create_datapack --cloud made it here:\n%s", out)
	}
	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "datapacks", code, 0, out+errs, "  team-notes  cloud\n")
	want(t, "datapacks", code, 0, out, "  default  local  (")

	const runbook = "the deploy runbook lives in the team wiki"
	code, out, errs = runSh("add_memory", "--data-dir", dir, runbook, "--datapack", "team-notes")
	want(t, "add_memory to a name only Cloud has", code, 0, out+errs, " to Meko Cloud datapack team-notes\n")
	code, out, errs = runSh("search", "--data-dir", cloudDir, runbook, "--datapack", "team-notes", "--source", "memory")
	want(t, "the memory is in Cloud", code, 0, out+errs, runbook+"  (meko_cli, ")
	code, out, errs = runSh("search", "--data-dir", dir, runbook, "--datapack", "cloud:team-notes", "--source", "memory")
	want(t, "search a Cloud datapack", code, 0, out+errs, runbook)
	code, out, _ = runSh("search", "--data-dir", dir, runbook, "--source", "memory")
	if code != 0 || strings.Contains(out, runbook) {
		t.Fatalf("the memory went to this Meko too: %d\n%s", code, out)
	}

	// A name both have is this Meko's; cloud: picks Meko Cloud's.
	for _, args := range [][]string{{"notes"}, {"notes", "--cloud"}} {
		if code, out, errs = runSh(append([]string{"create_datapack", "--data-dir", dir}, args...)...); code != 0 {
			t.Fatalf("create_datapack %v: %s%s", args, out, errs)
		}
	}
	code, out, errs = runSh("add_memory", "--data-dir", dir, "a fact kept here", "--datapack", "notes")
	if code != 0 || strings.Contains(out+errs, "Meko Cloud") {
		t.Fatalf("add_memory to a name both have: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runSh("add_memory", "--data-dir", dir, "a fact kept in Cloud", "--datapack", "cloud:notes")
	want(t, "add_memory to cloud:", code, 0, out+errs, " to Meko Cloud datapack notes\n")
	code, out, errs = runSh("search", "--data-dir", cloudDir, "a fact kept", "--datapack", "notes", "--source", "memory")
	want(t, "cloud: went to Cloud", code, 0, out+errs, "a fact kept in Cloud")
	if strings.Contains(out, "a fact kept here") {
		t.Fatalf("the name both have went to Cloud:\n%s", out)
	}

	// Files go to Cloud's knowledge base through its REST API.
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("# Deploys\n\nDeploys go out on Tuesdays.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runSh("upload", "--data-dir", dir, notes, "--datapack", "team-notes")
	want(t, "upload to Cloud", code, 0, out+errs, notes+": indexed in Meko Cloud datapack team-notes, 1 chunk (key ")
	code, out, errs = runSh("search", "--data-dir", cloudDir, "when do deploys go out", "--datapack", "team-notes",
		"--source", "knowledge_base")
	want(t, "the file is in Cloud", code, 0, out+errs, "Deploys go out on Tuesdays")
	code, out, errs = runSh("upload", "--data-dir", dir, notes, "--datapack", "team-notes", "--json")
	var offer struct{ Key string }
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &offer); code != 0 || err != nil {
		t.Fatalf("upload --json: %d %v\n%s%s", code, err, out, errs)
	}
	code, out, errs = runSh("upload_status", "--data-dir", dir, offer.Key, "--datapack", "cloud:team-notes")
	want(t, "upload_status in Cloud", code, 0, out+errs, offer.Key+": completed.")

	zip := filepath.Join(t.TempDir(), "team-notes.zip")
	code, _, errs = runSh("export", "--data-dir", dir, "team-notes", "-o", zip)
	want(t, "export a Cloud datapack", code, 1, errs,
		"export works only on this Meko's datapacks, and team-notes is in Meko Cloud")
	if _, err := os.Stat(zip); !os.IsNotExist(err) {
		t.Fatalf("export of a Cloud datapack wrote a file: %v", err)
	}

	// The prompt: use finds Cloud's datapack, later lines go there, and deleting it goes back to the
	// default.
	withStdin(t, "use team-notes\nadd_memory \"kept from the prompt\"\nuse\ndelete_datapack cloud:team-notes\ny\n"+
		"use\n")
	code, out, errs = runSh("--data-dir", dir)
	want(t, "the prompt's use of Cloud's", code, 0, out+errs, "Using Meko Cloud datapack team-notes\nAdded memory ")
	want(t, "the prompt's add_memory", code, 0, out, " to Meko Cloud datapack ")
	want(t, "the prompt's use, alone", code, 0, out, "\nUsing Meko Cloud datapack team-notes\n")
	want(t, "the prompt's delete_datapack", code, 0, out, "This deletes Meko Cloud datapack team-notes and "+
		"everything in it (memories, conversations, artifacts) for good\nDelete it? [y/N] \n"+
		"Deleted Meko Cloud datapack team-notes\nUsing the default datapack\nUsing the default datapack\n")
	if _, out, _ = runSh("datapacks", "--data-dir", cloudDir); strings.Contains(out, "team-notes") {
		t.Fatalf("the prompt's delete_datapack left it in Cloud:\n%s", out)
	}

	// At the prompt, use cloud:<id> shows Cloud's name, and deleting this Meko's datapack of the same
	// name leaves Cloud's in use; deleting Cloud's, named another way, goes back to the default.
	for _, args := range [][]string{{"pair"}, {"pair", "--cloud"}} {
		if code, out, errs = runSh(append([]string{"create_datapack", "--data-dir", dir}, args...)...); code != 0 {
			t.Fatalf("create_datapack %v: %s%s", args, out, errs)
		}
	}
	pairID := strings.TrimSuffix(out[strings.LastIndex(out, "(")+1:], ")\n")
	withStdin(t, "use cloud:"+pairID+"\ndelete_datapack pair\ny\nuse\ndelete_datapack cloud:pair\ny\nuse\n")
	code, out, errs = runSh("--data-dir", dir)
	want(t, "the prompt's use cloud:<id>", code, 0, out+errs, "Using Meko Cloud datapack pair\n")
	want(t, "the prompt keeps Cloud's after this Meko's is deleted", code, 0, out,
		"Deleted datapack pair\nUsing Meko Cloud datapack pair\n")
	want(t, "the prompt drops Cloud's once it's deleted", code, 0, out,
		"Deleted Meko Cloud datapack pair\nUsing the default datapack\nUsing the default datapack\n")

	withStdin(t, "y\n")
	code, out, errs = runSh("delete_datapack", "--data-dir", dir, "cloud:notes")
	want(t, "delete_datapack cloud:", code, 0, out+errs, "Deleted Meko Cloud datapack notes\n")
	if _, out, _ = runSh("datapacks", "--data-dir", dir, "--local"); !strings.Contains(out, "  notes") {
		t.Fatalf("delete_datapack cloud:notes deleted this Meko's:\n%s", out)
	}
	code, _, errs = runSh("search", "--data-dir", dir, "x", "--datapack", "cloud:nosuch")
	want(t, "a missing Cloud name", code, 1, errs, `no Meko Cloud datapack is named "nosuch"`)
	code, _, errs = runSh("search", "--data-dir", dir, "x", "--datapack", "nosuch")
	want(t, "a name missing on both sides", code, 1, errs,
		`no datapack is named "nosuch" here or in Meko Cloud (mekosh datapacks lists them)`)
}

// A key Meko Cloud refuses says so, without the key.
func TestCloudDatapacksKeyRefused(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")
	const stale = "mko_tkn_stale_0123456789abcd"
	if err := os.WriteFile(filepath.Join(dir, "secrets", "meko_cloud_key"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"search", "x", "--datapack", "team-runbooks"},
		{"create_datapack", "x", "--cloud"}} {
		code, out, errs = runSh(append(args, "--data-dir", dir)...)
		want(t, args[0]+" with a refused key", code, 1, errs,
			"mekosh: Meko Cloud refused the key. Link it again in Settings › Meko Cloud, or with meko cloud_link\n")
		if strings.Contains(out+errs, stale) {
			t.Fatalf("the key is in the output:\n%s%s", out, errs)
		}
	}
}

func TestSplitSide(t *testing.T) {
	for _, c := range []struct{ in, side, rest string }{
		{"notes", "", "notes"},
		{"cloud:notes", "cloud", "notes"},
		{"local:notes", "local", "notes"},
		{"cloud:", "cloud", ""},
		{"Cloud:notes", "", "Cloud:notes"},
	} {
		if side, rest := splitSide(c.in); side != c.side || rest != c.rest {
			t.Errorf("splitSide(%q) = %q, %q, want %q, %q", c.in, side, rest, c.side, c.rest)
		}
	}
	d := target{cloud: true, id: "c0ffee00-0000-4000-8000-000000000001", name: "team-notes"}
	if d.spec() != "cloud:"+d.id || d.noun() != "Meko Cloud datapack team-notes" {
		t.Errorf("a Cloud target: %q, %q", d.spec(), d.noun())
	}
	if got := statusHint(d, "k"); got != "mekosh upload_status k --datapack cloud:"+d.id {
		t.Errorf("statusHint = %q", got)
	}
	if got := statusHint(target{}, "k"); got != "mekosh upload_status k" {
		t.Errorf("statusHint of the default = %q", got)
	}
}

// postUpload takes any 2xx as a file received: Meko's link answers 204, and an S3 POST can be told
// to answer 200 or 201.
func TestPostUploadTakesAny2xx(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusCreated, http.StatusOK} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil || !strings.Contains(string(body), "name=\"key\"\r\n\r\nk\r\n") {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			w.WriteHeader(status)
		}))
		err := postUpload(t.Context(), ts.URL, map[string]string{"key": "k"}, "a.md", []byte("# a\n"))
		ts.Close()
		if err != nil {
			t.Errorf("%d: %v", status, err)
		}
	}
}
