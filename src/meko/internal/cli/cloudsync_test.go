package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// A bad flag is a usage error (exit 2, pointing at the help); --help alone exits 0.
func TestCloudSyncFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runCloudSync([]string{"--bogus"}, &out, &errOut); code != exitUsage ||
		!strings.Contains(errOut.String(), "See meko cloud_sync --help") {
		t.Errorf("--bogus: exit %d, stderr %q", code, errOut.String())
	}
	out.Reset()
	code := runCloudSync([]string{"--help"}, &out, &errOut)
	if code != exitOK || !strings.Contains(out.String(), "cloud_sync") {
		t.Errorf("--help: exit %d, %q", code, out.String())
	}
	if code := runCloudSync(nil, &out, &errOut); code != exitUsage {
		t.Errorf("no datapack: exit %d", code)
	}
}

// fakeSyncServer is a running Meko as meko cloud-sync sees it, holding one datapack, invoices, that
// fits the plan: it reports whether the sync itself was asked for.
func fakeSyncServer(t *testing.T) (dataDir string, synced *bool) {
	t.Helper()
	const id = "11111111-1111-4111-8111-111111111111"
	synced = new(bool)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/datapacks":
			reply([]map[string]string{{"datapack_id": id, "datapack_name": "invoices", "web_url": "/datapacks/" + id}})
		case "POST /api/v1/datapacks/" + id + "/cloud-sync/precheck":
			reply(map[string]any{"ok": true, "plan": "pro", "checks": []any{}})
		case "POST /api/v1/datapacks/" + id + "/cloud-sync":
			*synced = true
			reply(map[string]any{"state": "done", "web_url": "https://cloud.example/datapacks/x"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	dataDir = t.TempDir()
	l := paths.New(dataDir)
	u, _ := url.Parse(ts.URL)
	for path, text := range map[string]string{l.Port: u.Port(), l.Token: "admin-token"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, synced
}

// A sync deletes the datapack here, so it runs only once its exact name is typed, even when the
// command named it by id; --force asks nothing.
func TestCloudSyncAsksForTheName(t *testing.T) {
	t.Cleanup(func() { stdin = os.Stdin })
	for _, tc := range []struct {
		name, arg, typed string
		force, want      bool
	}{
		{"wrong name", "invoices", "Invoices\n", false, false},
		{"nothing typed", "invoices", "", false, false},
		{"its name", "invoices", "invoices\n", false, true},
		{"named by id, asked its name", "11111111-1111-4111-8111-111111111111", "invoices\n", false, true},
		{"force", "invoices", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, synced := fakeSyncServer(t)
			stdin = strings.NewReader(tc.typed)
			args := []string{tc.arg, "--data-dir", dir}
			if tc.force {
				args = append(args, "--force")
			}
			var out, errOut bytes.Buffer
			code := runCloudSync(args, &out, &errOut)
			if *synced != tc.want || (code == exitOK) != tc.want {
				t.Fatalf("synced %v, exit %d, want synced %v\nout: %s\nerr: %s", *synced, code, tc.want, &out, &errOut)
			}
			asked := strings.Contains(out.String(), "Type its name to confirm")
			if asked == tc.force {
				t.Errorf("asked %v with --force %v: %s", asked, tc.force, &out)
			}
			if !tc.want && !strings.Contains(errOut.String(), "wasn't synced") {
				t.Errorf("stderr %q", &errOut)
			}
		})
	}
}
