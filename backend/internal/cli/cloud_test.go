package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/paths"
)

const testCloudKey = "mko_tkn_cloud_0123456789wxyz"

// testCloudKey2 is another key fakeCloud's MCP server takes, for a new key.
const testCloudKey2 = "mko_tkn_cloud_9876543210abcd"

// fakeCloudDown makes fakeCloud's GET /api/v1/datapacks fail with a 500, and fakeCloudEmpty makes it
// list none.
var fakeCloudDown, fakeCloudEmpty atomic.Bool

// fakeCloudRows are fakeCloud's two datapacks, as its datapack_list tool and GET /datapacks give them.
const fakeCloudRows = `[{"datapack_id":"c0ffee00-0000-4000-8000-000000000001",` +
	`"datapack_name":"team-runbooks","grant":"viewer","created_at":"2026-10-02T00:00:00Z",` +
	`"datapack_ysql_connection_string":"postgresql://db.example.com/dp"},` +
	`{"datapack_id":"c0ffee00-0000-4000-8000-000000000002","datapack_name":"cloud-notes",` +
	`"description":"mine","grant":"owner","created_at":"2026-10-01T00:00:00Z"}]`

// fakeCloud is a Meko server for the link's checks: MCP at /mcp, taking testCloudKey (or testCloudKey2), with a
// datapack_list tool, and the REST API's envelope at /api/v1, where GET /datapacks gives two rows to testCloudKey.
func fakeCloud(t *testing.T) string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "datapack_list", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text := fakeCloudRows
			switch {
			case fakeCloudDown.Load():
				text = `{"error":"list_failed","detail":"Cloud is down."}`
			case fakeCloudEmpty.Load():
				text = "[]"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
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
			case fakeCloudEmpty.Load():
				_, _ = w.Write([]byte(`{"data":[]}`))
			default:
				_, _ = w.Write([]byte(`{"data":` + fakeCloudRows + `}`))
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		if fakeSignIn(w, r) {
			return
		}
		if a := r.Header.Get("Authorization"); a != "Bearer "+testCloudKey && a != "Bearer "+testCloudKey2 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// fakeCloudMints is the key fakeCloud's /api-keys makes for a sign-in: testCloudKey2 unless set.
var fakeCloudMints atomic.Value

// fakeSignIn serves fakeCloud's browser sign-in (docs/cloud.md): discovery, client registration, an
// /authorize that signs in at once and sends the browser back with a code, /token and /api-keys.
func fakeSignIn(w http.ResponseWriter, r *http.Request) bool {
	base := "http://" + r.Host
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		meta := `{"authorization_endpoint":"` + base + `/authorize","token_endpoint":"` + base +
			`/token","registration_endpoint":"` + base + `/register"}`
		_, _ = w.Write([]byte(meta)) //nolint:gosec // G705: a test fake's JSON
	case "/register":
		_, _ = w.Write([]byte(`{"client_id":"client-1"}`))
	case "/authorize":
		q := r.URL.Query()
		//nolint:gosec // G710: a test fake of Meko Cloud, sending the browser back as Meko Cloud does
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=the-code&state="+url.QueryEscape(q.Get("state")),
			http.StatusFound)
	case "/token":
		_, _ = w.Write([]byte(`{"access_token":"the-token"}`))
	case "/api-keys":
		key, _ := fakeCloudMints.Load().(string)
		if key == "" {
			key = testCloudKey2
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"api_key":"` + key + `","name":"Meko on test"}`))
	default:
		return false
	}
	return true
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
	wantCloud := cloudToken{Set: true, Linked: true, WebURL: cloud,
		Account: cloudlink.Link{URL: cloud + "/mcp", Key: testCloudKey}.Account(), DatapackRouting: true}
	if len(c.Account) != 16 {
		t.Fatalf("token --json account = %q", c.Account)
	}
	if c != wantCloud {
		t.Fatalf("token --json cloud = %+v, want %+v", c, wantCloud)
	}
	// The key stays in Meko: extensions reach Meko Cloud through it.
	if _, js, _ := run("token", "--data-dir", dir, "--json"); strings.Contains(js, testCloudKey) ||
		strings.Contains(js, "api_key") {
		t.Fatalf("token --json shows the key: %s", js)
	}
	code, out, _ = run("status", "--data-dir", dir, "--verbose")
	want(t, "status", code, exitNotRunning, out, "Meko Cloud:    linked to "+cloud+"/mcp (key …wxyz)")

	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--off")
	want(t, "cloud_link --off through the server", code, 0, out+errs, "Meko Cloud is unlinked")
	if c := readCloudToken(t, dir); c != (cloudToken{Set: true, DatapackRouting: true}) {
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
	if c := readCloudToken(t, dir); c.Error == "" || c.Set || c.Linked {
		t.Fatalf("an unreadable link: %+v", c)
	}
}

// asTerminal makes meko cloud_link take its input as a person's at a terminal, and openBrowser call
// browse with each address it's asked to open.
func asTerminal(t *testing.T, browse func(string)) {
	t.Helper()
	term, open := terminalIn, openBrowser
	terminalIn = func(io.Reader) bool { return true }
	openBrowser = func(u string) error {
		if browse != nil {
			browse(u)
		}
		return nil
	}
	t.Cleanup(func() { terminalIn, openBrowser = term, open })
}

// followTo is a browser that signs in at once: it follows the sign-in address to the callback.
func followTo(t *testing.T) func(string) {
	return func(u string) {
		res, err := http.Get(u) //nolint:gosec,noctx // the test's own fake Cloud
		if err != nil {
			t.Error(err)
			return
		}
		_ = res.Body.Close()
	}
}

// On a terminal, meko cloud_link asks how to link: signing in with the browser (Enter) makes a key
// and links with it, as the Meko installer does; 2 takes a key. A browser on another computer
// finishes by pasting the address it ended on, and a key the link then refuses is named, since
// Meko Cloud can't delete it.
func TestCloudLinkSignIn(t *testing.T) {
	dir, _ := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)
	asTerminal(t, followTo(t))

	withStdin(t, "\n")
	code, out, errs := run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "sign in with the browser", code, 0, out+errs, "Linked to "+cloud+"/mcp (key …abcd)")
	want(t, "the choice", code, 0, errs, "1) Sign in with your browser\n  2) Enter an API key\n")
	want(t, "the browser", code, 0, errs, "Opened your browser to sign in to Meko Cloud.")
	if strings.Contains(out+errs, "the-code") || strings.Contains(out+errs, "the-token") ||
		strings.Contains(out+errs, testCloudKey2) {
		t.Fatalf("the output holds a credential:\n%s%s", out, errs)
	}
	if c := readCloudToken(t, dir); c.Account != (cloudlink.Link{URL: cloud + "/mcp", Key: testCloudKey2}).Account() {
		t.Fatalf("linked with the minted key? %+v", c)
	}

	withStdin(t, "2\n"+testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir)
	want(t, "enter a key", code, 0, out+errs, "Linked to "+cloud+"/mcp (key …wxyz)")

	withStdin(t, "3\n")
	code, _, errs = run("cloud_link", "--data-dir", dir)
	want(t, "neither", code, 1, errs, "choose 1 to sign in with your browser, or 2 to enter an API key")

	withStdin(t, "\n")
	code, _, errs = run("cloud_link", "--data-dir", dir, "--url", "not a url")
	want(t, "a bad URL, signing in", code, 1, errs, "isn't a URL. It should look like https://meko.example.com\n")

	// The browser is elsewhere: its redirect never reaches this computer, and its address is pasted.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	stdin = pr
	t.Cleanup(func() { stdin = os.Stdin })
	go func() { _, _ = io.WriteString(pw, "1\n") }()
	asTerminal(t, func(u string) {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Get(u) //nolint:noctx,gosec // G704: the test's own fake Cloud
		if err != nil {
			t.Error(err)
			return
		}
		_ = res.Body.Close()
		go func() { _, _ = io.WriteString(pw, res.Header.Get("Location")+"\n") }()
	})
	code, out, errs = run("cloud_link", "--data-dir", dir)
	want(t, "a pasted address", code, 0, out+errs, "Linked to "+cloud+"/mcp (key …abcd)")
	want(t, "the paste prompt", code, 0, errs, "paste the address it ended on here")

	// A key the link refuses stays in the account, and the error names it.
	fakeCloudMints.Store("mko_tkn_refused_0000")
	t.Cleanup(func() { fakeCloudMints.Store("") })
	asTerminal(t, followTo(t))
	withStdin(t, "1\n")
	code, _, errs = run("cloud_link", "--data-dir", dir)
	want(t, "a refused key", code, 1, errs, `The key Meko made, "Meko on test", is still in your Meko Cloud account`)
	fakeCloudMints.Store("")

	// --no-browser prints the address; nobody signs in, so it times out.
	wait := signInWait
	signInWait = 100 * time.Millisecond
	t.Cleanup(func() { signInWait = wait })
	opened := false
	asTerminal(t, func(string) { opened = true })
	withStdin(t, "\n")
	code, _, errs = run("cloud_link", "--data-dir", dir, "--no-browser")
	want(t, "--no-browser", code, 1, errs,
		"Open this address in a browser to sign in to Meko Cloud:\n  "+cloud+"/authorize?")
	want(t, "no sign-in", code, 1, errs, "timed out after 100ms waiting for the sign-in")
	if opened {
		t.Fatal("--no-browser opened the browser")
	}
	code, _, errs = run("cloud_link", "--data-dir", dir, "--off", "--no-browser")
	want(t, "--off --no-browser", code, exitUsage, errs, "--off and --no-browser don't go together")
}

// meko-cli datapacks lists the datapacks /mcp lists: this Meko's while unlinked, and the linked Meko
// Cloud's with this Meko's while it's linked, as names and descriptions by name (ids are --json's,
// the tool's reply as it is). A Meko Cloud that fails says so.
func TestDatapacksLinked(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")

	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "unlinked", code, 0, out, "default   Created by Meko on first start.\n")
	if errs != "" {
		t.Fatalf("unlinked: %s", errs)
	}

	withStdin(t, testCloudKey+"\n")
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")

	code, out, errs = runSh("datapacks", "--data-dir", dir)
	if code != 0 || errs != "" ||
		out != "cloud-notes     mine\ndefault         Created by Meko on first start.\nteam-runbooks\n" {
		t.Fatalf("linked, both by name: %d\n%s%s", code, out, errs)
	}
	code, out, _ = runSh("datapacks", "--data-dir", dir, "--json")
	if code != 0 || !strings.Contains(out, `"team-runbooks"`) || !strings.Contains(out, `"default"`) ||
		strings.Contains(out, "location") {
		t.Fatalf("--json is the tool's reply: %d\n%s", code, out)
	}

	fakeCloudDown.Store(true)
	t.Cleanup(func() { fakeCloudDown.Store(false) })
	code, _, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "Cloud down", code, 1, errs, "meko-cli: list_failed: Cloud is down")
	fakeCloudDown.Store(false)

	fakeCloudEmpty.Store(true)
	t.Cleanup(func() { fakeCloudEmpty.Store(false) })
	if code, out, errs = runSh("datapacks", "--data-dir", dir); code != 0 ||
		out != "default   Created by Meko on first start.\n" || errs != "" {
		t.Fatalf("no datapacks in Meko Cloud, linked: %d\n%s%s", code, out, errs)
	}
}

// While Meko Cloud is linked, this Meko's /mcp works on its datapacks and on this Meko's own, each by
// name or id with nothing saying which is which, and new datapacks are made there. The "Meko Cloud"
// here is a second Meko server, which serves the MCP tools and REST routes with Cloud's shapes:
// meko-cli's commands, an agent's MCP session with the client token, REST's creates and the client
// token's REST calls all reach it, and unlinking leaves this Meko's own datapacks alone.
func TestCloudDatapacks(t *testing.T) {
	withMemoryEngine(t)
	t.Setenv("MEKO_SECRETS", "file")
	dir, port := setup(t)
	startReady(t, dir, port)
	cloudDir, cloudPort := shortDir(t), strconv.Itoa(freePort(t))
	t.Setenv("MEKO_WEB_PORT", strconv.Itoa(freePort(t)))
	t.Cleanup(func() { run("stop", "--data-dir", cloudDir, "--keep-extensions") })
	startReady(t, cloudDir, cloudPort)

	code, _, errs := runSh("search", "--data-dir", dir, "x", "--datapack", "nope")
	want(t, "a missing name", code, 1, errs, `no datapack is named "nope" (meko-cli datapacks lists them)`)
	code, out, errs := runSh("create_datapack", "--data-dir", dir, "kept-here")
	want(t, "create_datapack unlinked", code, 0, out+errs, "Created datapack kept-here (")

	_, key, _ := run("token", "--data-dir", cloudDir)
	withStdin(t, key)
	cloud := "http://127.0.0.1:" + cloudPort
	code, out, errs = run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--api-url", cloud+"/api/v1")
	want(t, "cloud_link", code, 0, out+errs, "Linked to "+cloud+"/mcp")

	code, out, errs = runSh("create_datapack", "--data-dir", dir, "team-notes")
	want(t, "create_datapack linked", code, 0, out+errs, "Created datapack team-notes (")
	if _, out, _ = runSh("datapacks", "--data-dir", cloudDir); !strings.Contains(out, "team-notes") {
		t.Fatalf("create_datapack didn't make it in Cloud:\n%s", out)
	}
	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "datapacks linked", code, 0, "\n"+out+errs, "\nteam-notes\n")
	want(t, "datapacks linked lists this Meko's", code, 0, "\n"+out+errs, "\nkept-here\n")

	const runbook = "the deploy runbook lives in the team wiki"
	code, out, errs = runSh("add_memory", "--data-dir", dir, runbook, "--datapack", "team-notes")
	want(t, "add_memory", code, 0, out+errs, "Added memory ")
	code, out, errs = runSh("search", "--data-dir", cloudDir, runbook, "--datapack", "team-notes", "--source", "memory")
	want(t, "the memory is in Cloud", code, 0, out+errs, runbook+"  (meko_cli, ")
	code, out, errs = runSh("search", "--data-dir", dir, runbook, "--datapack", "team-notes", "--source", "memory")
	want(t, "search a Cloud datapack", code, 0, out+errs, runbook)

	// A datapack made here before the link is still served here, by name.
	const oncall = "the on-call rota is in the shared calendar"
	code, out, errs = runSh("add_memory", "--data-dir", dir, oncall, "--datapack", "kept-here")
	want(t, "add_memory to this Meko's, linked", code, 0, out+errs, "Added memory ")
	code, out, errs = runSh("search", "--data-dir", dir, oncall, "--datapack", "kept-here", "--source", "memory")
	want(t, "search this Meko's, linked", code, 0, out+errs, oncall)
	if _, out, _ = runSh("datapacks", "--data-dir", cloudDir); strings.Contains(out, "kept-here") {
		t.Fatalf("this Meko's datapack reached Cloud:\n%s", out)
	}
	dpHere := datapackID(t, dir, "kept-here")

	// Files go to Cloud's knowledge base.
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("# Deploys\n\nDeploys go out on Tuesdays.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runSh("upload", "--data-dir", dir, notes, "--datapack", "team-notes")
	want(t, "upload to Cloud", code, 0, out+errs, notes+": indexed, 1 chunk (key ")
	code, out, errs = runSh("upload", "--data-dir", dir, notes, "--datapack", "team-notes", "--json")
	var offer struct{ Key string }
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &offer); code != 0 || err != nil {
		t.Fatalf("upload --json: %d %v\n%s%s", code, err, out, errs)
	}
	code, out, errs = runSh("upload_status", "--data-dir", dir, offer.Key, "--datapack", "team-notes")
	want(t, "upload_status in Cloud", code, 0, out+errs, offer.Key+": completed.")

	// An agent's session with the client token: Meko Cloud's tools (the second Meko's, so its
	// datapack_metrics too), and this Meko's knowledgebase_upload_file reading a file here.
	l := paths.New(dir)
	_, clientTok, _ := run("token", "--data-dir", dir)
	cs := clientSession(t, "http://127.0.0.1:"+port+"/mcp", strings.TrimSpace(clientTok))
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["knowledgebase_upload_file"] || !names["context_search"] || !names["datapack_metrics"] {
		t.Fatalf("linked tools/list: %v", names)
	}
	dp := datapackID(t, dir, "team-notes")
	// An id is looked up by itself (datapack_describe), not in a list of every datapack.
	code, out, errs = runSh("search", "--data-dir", dir, runbook, "--datapack", dp, "--source", "memory")
	want(t, "search a Cloud datapack by id", code, 0, out+errs, runbook)
	code, _, errs = runSh("search", "--data-dir", dir, "x", "--datapack", "c0ffee00-0000-4000-8000-0000000000ff")
	want(t, "a missing id", code, 1, errs, "no datapack has the id c0ffee00-0000-4000-8000-0000000000ff")
	faq := filepath.Join(t.TempDir(), "faq.md")
	if err = os.WriteFile(faq, []byte("# FAQ\n\nThe staging cluster restarts at noon.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text := callText(t, cs, "knowledgebase_upload_file", map[string]any{"path": faq, "datapack_id": dp,
		"conversation_id": "c0ffee0000004000800000000000000a"})
	if !strings.Contains(text, `"already_uploaded":false`) || !strings.Contains(text, `"document_key"`) {
		t.Fatalf("knowledgebase_upload_file to Cloud: %s", text)
	}
	text = callText(t, cs, "datapack_list", map[string]any{"conversation_id": "c0ffee0000004000800000000000000a"})
	if !strings.Contains(text, "team-notes") || !strings.Contains(text, "kept-here") ||
		strings.Contains(text, "location") {
		t.Fatalf("datapack_list through this Meko, linked: %s", text)
	}

	// REST: a create goes to Cloud, whatever location it names.
	c, err := newClient(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rest-made", "x-local"} {
		var made map[string]any
		if err = c.send(t.Context(), http.MethodPost, "/api/v1/datapacks", map[string]string{"name": name,
			"location": "local"}, &made); err != nil || made["location"] != nil {
			t.Fatalf("REST create linked: %v %+v", err, made)
		}
	}
	if _, out, _ = runSh("datapacks", "--data-dir", cloudDir); !strings.Contains(out, "x-local") {
		t.Fatalf("REST's create didn't make it in Cloud:\n%s", out)
	}
	// REST with the client token, as extensions call it: /api/v1 serves this Meko's datapacks and
	// passes Meko Cloud's on to it, and /api/v1/cloud is gone. AgentK's knowledge upload works on
	// each side: the upload link comes from the Meko that has the datapack, and its status reads
	// completed there.
	tok := strings.TrimSpace(clientTok)
	base := "http://127.0.0.1:" + port + "/api/v1"
	if status, body := restAs(t, tok, http.MethodGet, base+"/cloud/datapacks", nil); status != 404 {
		t.Fatalf("/api/v1/cloud/datapacks: %d %s", status, body)
	}
	for _, side := range []struct{ name, id, port string }{{"this Meko", dpHere, port}, {"Cloud", dp, cloudPort}} {
		kb := "http://127.0.0.1:" + port + "/api/v1/datapacks/" + side.id + "/knowledge-bases"
		file := []byte("# Releases\n\nReleases on " + side.name + " are cut on Mondays.\n")
		sum := sha256.Sum256(file)
		status, body := restAs(t, tok, http.MethodPost, kb+"/upload-url", map[string]any{"filename": "releases.md",
			"size": len(file), "content_hash": hex.EncodeToString(sum[:]), "client": "agent-k-local"})
		var offer struct {
			Data struct {
				URL    string            `json:"url"`
				Key    string            `json:"key"`
				Fields map[string]string `json:"fields"`
			} `json:"data"`
		}
		if err = json.Unmarshal([]byte(body), &offer); status != 200 || err != nil ||
			!strings.HasPrefix(offer.Data.URL, "http://127.0.0.1:"+side.port+"/") {
			t.Fatalf("upload-url on %s: %d %s", side.name, status, body)
		}
		postFile(t, offer.Data.URL, offer.Data.Fields, "releases.md", file)
		state := ""
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			_, body = restAs(t, tok, http.MethodGet, kb+"/document-status?document_key="+url.QueryEscape(offer.Data.Key), nil)
			var st struct{ Data struct{ State string } }
			_ = json.Unmarshal([]byte(body), &st)
			if state = st.Data.State; state == "completed" || state == "failed" {
				break
			}
		}
		if state != "completed" {
			t.Fatalf("the upload on %s: %q", side.name, state)
		}
	}
	// Only Meko Cloud shares: this Meko's datapack says why it can't be, and Cloud's routes pass on.
	status, body := restAs(t, tok, http.MethodGet,
		"http://127.0.0.1:"+port+"/api/v1/datapacks/"+dpHere+"/member-list", nil)
	if status != http.StatusConflict || !strings.Contains(body, `"DATAPACK_LOCAL"`) {
		t.Fatalf("member-list of this Meko's datapack: %d %s", status, body)
	}
	// The second Meko has no sharing routes of its own: unlinked, it answers that it isn't linked.
	status, body = restAs(t, tok, http.MethodGet, "http://127.0.0.1:"+port+"/api/v1/datapacks/incoming-shares", nil)
	if status != http.StatusConflict || !strings.Contains(body, `"CLOUD_NOT_LINKED"`) {
		t.Fatalf("incoming-shares passed on: %d %s", status, body)
	}

	// export still works on this Meko's datapacks, by name; Cloud's can't be exported.
	zip := filepath.Join(t.TempDir(), "kept-here.zip")
	code, out, errs = runSh("export", "--data-dir", dir, "kept-here", "-o", zip)
	want(t, "export this Meko's, linked", code, 0, out+errs, "Exported to "+zip)
	code, _, errs = runSh("export", "--data-dir", dir, "team-notes")
	want(t, "export a Cloud datapack", code, 1, errs,
		`"team-notes" isn't one of this computer's datapacks: only those can be exported`)
	code, _, errs = runSh("export", "--data-dir", dir)
	want(t, "export with no name, linked", code, 1, errs, "name this computer's datapack to export")

	// delete_datapack of Cloud's datapack, by name: it asks, then deletes it there.
	withStdin(t, "y\n")
	code, out, _ = runSh("delete_datapack", "--data-dir", dir, "team-notes")
	want(t, "delete_datapack of Cloud's datapack", code, 0, out, "This deletes datapack team-notes and "+
		"everything in it (memories, conversations, artifacts) for good\nDelete it? [y/N] \n"+
		"Deleted datapack team-notes\n")
	if _, out, _ = runSh("datapacks", "--data-dir", cloudDir); strings.Contains(out, "team-notes") {
		t.Fatalf("delete_datapack left it in Cloud:\n%s", out)
	}

	// With Meko Cloud down, this Meko's datapacks still work, export by name included.
	if code, out, errs = run("stop", "--data-dir", cloudDir, "--keep-extensions"); code != 0 {
		t.Fatalf("stop Cloud: %s%s", out, errs)
	}
	code, out, errs = runSh("export", "--data-dir", dir, "kept-here", "-o", zip, "--force")
	want(t, "export with Meko Cloud down", code, 0, out+errs, "Exported to "+zip)
	code, out, errs = runSh("search", "--data-dir", dir, oncall, "--datapack", dpHere, "--source", "memory")
	want(t, "search this Meko's by id with Meko Cloud down", code, 0, out+errs, oncall)

	// Unlinked, this Meko's datapacks alone.
	code, out, errs = run("cloud_link", "--data-dir", dir, "--off")
	want(t, "cloud_link --off", code, 0, out+errs, "unlinked")
	code, out, errs = runSh("datapacks", "--data-dir", dir)
	want(t, "datapacks unlinked", code, 0, "\n"+out+errs, "\nkept-here\n")
	if strings.Contains(out, "rest-made") {
		t.Fatalf("unlinked, Cloud's datapacks are listed:\n%s", out)
	}
}

// clientSession is an MCP session with url, sending tok as an agent does.
func clientSession(t *testing.T, url, tok string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url,
		HTTPClient: &http.Client{Transport: bearer{tok}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callText calls a tool and returns its reply's text.
func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if res.IsError {
		t.Fatalf("%s failed: %s", name, text)
	}
	return text
}

// datapackID is the id of the datapack named in the Meko at dir, through meko-cli datapacks --json.
func datapackID(t *testing.T, dir, name string) string {
	t.Helper()
	_, out, _ := runSh("datapacks", "--data-dir", dir, "--json")
	var rows []datapackRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("datapacks --json: %v\n%s", err, out)
	}
	r, err := pick(rows, name, "datapacks")
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// A key Meko Cloud refuses says so, without the key.
func TestCloudDatapacksKeyRefused(t *testing.T) {
	dir, port := setup(t)
	t.Setenv("MEKO_SECRETS", "file")
	cloud := fakeCloud(t)
	withStdin(t, testCloudKey+"\n")
	code, out, errs := run("cloud_link", "--data-dir", dir, "--url", cloud+"/mcp", "--web-url", cloud)
	want(t, "cloud_link", code, 0, out+errs, "Linked to")
	// The key changes behind Meko's back (the server reads it at start).
	const stale = "mko_tkn_stale_0123456789abcd"
	if err := os.WriteFile(filepath.Join(dir, "secrets", "meko_cloud_key"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	for _, args := range [][]string{{"datapacks"}, {"create_datapack", "x"}} {
		code, out, errs = runSh(append(args, "--data-dir", dir)...)
		want(t, args[0]+" with a refused key", code, 1, errs,
			"meko-cli: Meko Cloud refused the key. Link it again in Settings › Meko Cloud, or with meko cloud_link\n")
		if strings.Contains(out+errs, stale) {
			t.Fatalf("the key is in the output:\n%s%s", out, errs)
		}
	}
}

func TestTarget(t *testing.T) {
	d := target{id: "c0ffee00-0000-4000-8000-000000000001", name: "team-notes"}
	if d.noun() != "datapack team-notes" {
		t.Errorf("noun: %q", d.noun())
	}
	if got := statusHint(d, "k"); got != "meko-cli upload_status k --datapack "+d.id {
		t.Errorf("statusHint = %q", got)
	}
	if got := statusHint(target{}, "k"); got != "meko-cli upload_status k" {
		t.Errorf("statusHint of the default = %q", got)
	}
	rows := []datapackRow{{ID: "a", Name: "notes"}, {ID: "b", Name: "twin"}, {ID: "c", Name: "twin"}}
	if r, err := pick(rows, "A", "x"); err != nil || r.Name != "notes" {
		t.Errorf("pick by id: %+v %v", r, err)
	}
	if _, err := pick(rows, "twin", "x"); err == nil || !strings.Contains(err.Error(), "2 datapacks are named") {
		t.Errorf("pick of a name two have: %v", err)
	}
	if _, err := pick(rows, "nope", "x"); err == nil || !strings.Contains(err.Error(), `no datapack is named "nope"`) {
		t.Errorf("pick of a missing name: %v", err)
	}
}

// restAs calls this Meko's REST API at link with a token, as an extension does, and returns the
// status and body.
func restAs(t *testing.T, tok, method, link string, body any) (int, string) {
	t.Helper()
	var in io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		in = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, link, in)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res.StatusCode, string(out)
}

// postFile posts a file to an upload link as AgentK does: the signed fields, then the file, no key.
func postFile(t *testing.T, link string, fields map[string]string, name string, data []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = w.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, link, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		t.Fatalf("the upload link answered %d", res.StatusCode)
	}
}
