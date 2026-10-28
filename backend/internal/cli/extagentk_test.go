package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/update"
)

// fakeAgentK is an AgentK release's bin/agentk, keeping its state in its folder in the data folder
// (MEKO_HOME, which Meko sets): install, start, stop and restart log to its log with its version,
// status succeeds while it runs, and selftest passes. With rollback-to there, restart does what
// meko _launch does after three failed starts: points app/current back at that version.
func fakeAgentK(version string) string {
	return `#!/bin/sh
s="$MEKO_HOME/extensions/agentk"
case "$1" in
  version) echo '{"version":"` + version + `"}' ;;
  selftest) echo "selftest ok" ;;
  install) echo "$*" >> "$s/log" ;;
  status) [ -f "$s/running" ] ;;
  start) touch "$s/running"; echo "start ` + version + `" >> "$s/log" ;;
  stop) rm -f "$s/running"; echo "stop ` + version + `" >> "$s/log" ;;
  restart) echo "restart ` + version + `" >> "$s/log"
    [ ! -f "$s/rollback-to" ] || ln -sfn "$(cat "$s/rollback-to")" "$s/app/current" ;;
  _handoff) printf '{"socket": "%s"}\n' "$s/web.sock" ;;
esac
`
}

// packageTarball is <pkg>-<version>-<os>-<arch>.tar.gz holding bin/<command> with body.
func packageTarball(t *testing.T, pkg, command, version, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: pkg + "-" + version + "/bin/" + command, Mode: 0o755,
		Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeRelease is a release to serve: its tag, and the package and version of the tarball it carries.
type fakeRelease struct {
	tag, pkg, version string
	tgz               []byte
}

// serveReleases serves a GitHub API with rels for this platform (and Meko's own, with no tarball
// for any extension), and makes the CLI use it.
func serveReleases(t *testing.T, rels ...fakeRelease) {
	t.Helper()
	goos, arch := update.Platform()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/releases" {
			list := make([]update.Release, 0, 1+len(rels))
			list = append(list, update.Release{Tag: "meko-v0.20261031.0", Assets: []update.Asset{{Name: "install.sh"}}})
			for i, rel := range rels {
				list = append(list, update.Release{Tag: rel.tag, URL: srv.URL + "/page/" + rel.tag, Assets: []update.Asset{
					{Name: update.PackageTarball(rel.pkg, rel.version, goos, arch), BrowserURL: fmt.Sprintf("%s/tar/%d", srv.URL, i)},
					{Name: update.Sums, BrowserURL: fmt.Sprintf("%s/sums/%d", srv.URL, i)},
				}})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/tar/%d", &i); err == nil && i < len(rels) {
			_, _ = w.Write(rels[i].tgz)
			return
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/sums/%d", &i); err == nil && i < len(rels) {
			h := sha256.Sum256(rels[i].tgz)
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(h[:]),
				update.PackageTarball(rels[i].pkg, rels[i].version, goos, arch))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	old := newReleaseClient
	newReleaseClient = func(string) *update.Client { return &update.Client{Repo: "o/r", API: srv.URL, HTTP: srv.Client()} }
	t.Cleanup(func() { newReleaseClient = old })
}

func agentkRelease(t *testing.T, tag, version string) fakeRelease {
	return fakeRelease{tag: tag, pkg: "agentk", version: version,
		tgz: packageTarball(t, "agentk", "agentk", version, fakeAgentK(version))}
}

// installedAgentK is a release install of AgentK's version, as install_extension leaves it.
func installedAgentK(t *testing.T, dir, version string) update.App {
	t.Helper()
	app := extensions.Known["agentk"].App(dir, "agentk")
	if err := os.MkdirAll(filepath.Join(app.Dir(version), "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(app.Exe(version), []byte(fakeAgentK(version)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Dir(version), "VERSION"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Repoint(version); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	link, err := app.LinkCommand(home)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: link, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	return app
}

// install_extension agentk installs AgentK's newest release as it does Presenter's (no installer of
// AgentK's), then runs its install with this Meko's URL and key and what followed --. --version
// installs another release, under either tag prefix.
func TestInstallExtensionInstallsAgentKsRelease(t *testing.T) {
	const newest, older = "0.20261030.0", "0.20261029.0"
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	serveReleases(t, agentkRelease(t, "agentk-v"+newest, newest), agentkRelease(t, "v"+older, older))
	code, out, errs = run("install_extension", "agentk", "--data-dir", dir, "--", "--agent", "kiro")
	want(t, "install_extension", code, 0, out+errs,
		"  ✓ Release         AgentK "+newest+" (agentk-v"+newest+"), downloaded and checked")
	app := extensions.Known["agentk"].App(dir, "agentk")
	if app.Current() != newest {
		t.Errorf("current %q", app.Current())
	}
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "agentk")
	reg, _ := extensions.Load(dir)
	if reg.Entries["agentk"].Command != link {
		t.Errorf("registry %+v", reg.Entries)
	}
	_, tok, _ := run("token", "--data-dir", dir)
	log := filepath.Join(extensions.Dir(dir, "agentk"), "log")
	wantArgs := "install --meko-url http://127.0.0.1:" + port + "/mcp --meko-key " + strings.TrimSpace(tok) +
		" --non-interactive --agent kiro\n"
	if got := readLog(t, log); got != wantArgs {
		t.Errorf("agentk got %q, want %q", got, wantArgs)
	}
	code, out, errs = run("install_extension", "agentk", "--version", older, "--data-dir", dir)
	want(t, "install_extension --version", code, 0, out+errs,
		"  ✓ Release         AgentK "+older+" (v"+older+"), downloaded and checked")
	if app.Current() != older {
		t.Errorf("current %q after --version", app.Current())
	}
	code, _, errs = run("install_extension", "agentk", "--version", "0.20261001.0", "--data-dir", dir)
	want(t, "a version with no release", code, 1, errs, "no release 0.20261001.0 of AgentK")
	// Going back to a version keeps Update automatically off, so the next check doesn't undo it.
	code, out, errs = run("update_extension", "agentk", "--auto=off", "--data-dir", dir)
	want(t, "--auto=off", code, 0, out+errs, "only reports AgentK's updates")
	code, out, errs = run("install_extension", "agentk", "--version", newest, "--data-dir", dir)
	want(t, "install_extension --version again", code, 0, out+errs, "  ✓ Release         AgentK "+newest)
	if reg, _ = extensions.Load(dir); reg.Entries["agentk"].Auto() {
		t.Errorf("the switch came back on: %+v", reg.Entries["agentk"])
	}
}

// An update of a running AgentK switches while it runs and restarts it with agentk restart, never
// stop and start (which would end its sessions), after checking the release with its selftest.
func TestUpdateExtensionRestartsAgentK(t *testing.T) {
	const old, next = "0.20261024.0", "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedAgentK(t, dir, old)
	state := extensions.Dir(dir, "agentk")
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	serveReleases(t, agentkRelease(t, "agentk-v"+next, next))
	code, out, errs := run("update_extension", "agentk", "--data-dir", dir)
	want(t, "update_extension", code, 0, out+errs, "AgentK is running "+next)
	if got := readLog(t, filepath.Join(state, "log")); got != "restart "+next+"\n" {
		t.Errorf("log %q", got)
	}
	if p, _ := app.ReadPending(); app.Current() != next || p == nil || p.From != old || p.To != next {
		t.Errorf("current %q, pending %+v", app.Current(), p)
	}
}

// When meko _launch rolls the new version back while agentk restart waits, the update says so.
func TestUpdateExtensionReportsAgentKsRollBack(t *testing.T) {
	const old, next = "0.20261023.0", "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedAgentK(t, dir, old)
	state := extensions.Dir(dir, "agentk")
	for f, body := range map[string]string{"running": "", "rollback-to": old} {
		if err := os.WriteFile(filepath.Join(state, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	serveReleases(t, agentkRelease(t, "agentk-v"+next, next))
	code, _, errs := run("update_extension", "agentk", "--data-dir", dir)
	want(t, "update_extension", code, 1, errs, "rolled back to "+old+" and won't be offered again")
}

// An AgentK still in its old home isn't one Meko updates until it has moved.
func TestUpdateExtensionWaitsForAgentKsMove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	noRealExtensions(t)
	dir := shortDir(t)
	exe := filepath.Join(home, ".agent-k-local", "app", "0.20261020.0", "bin", "agentk")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(fakeAgentK("0.20261020.0")), 0o700); err != nil { //nolint:gosec // G306: test
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["agentk"] = extensions.Entry{Command: exe}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("update_extension", "agentk", "--data-dir", dir)
	want(t, "an unmoved AgentK", code, 1, errs, "once it has moved into Meko's data folder")
	if u := testListener(t, dir).ExtensionUpdates("agentk"); u.Updatable ||
		!strings.Contains(u.NotUpdatable, "once it has moved into Meko's data folder") {
		t.Errorf("ExtensionUpdates = %+v", u)
	}
}

// AgentK's own [update] channel = "off" turns Update automatically off, saved, so it stays off once
// AgentK's config no longer says so.
func TestAgentKsOldUpdatesOffCarriesOver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedAgentK(t, dir, installedVersion)
	cfg := filepath.Join(extensions.Dir(dir, "agentk"), "config.toml")
	if err := os.WriteFile(cfg, []byte("[update]\nchannel = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := testListener(t, dir)
	if u := w.ExtensionUpdates("agentk"); u.AutoUpdate {
		t.Errorf("on with AgentK's updates off: %+v", u)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if reg, _ := extensions.Load(dir); reg.Entries["agentk"].Auto() || w.ExtensionUpdates("agentk").AutoUpdate {
		t.Errorf("came back on: %+v", reg.Entries["agentk"])
	}
}

// update_extension --all goes through every installed extension; --auto sets Update automatically.
func TestUpdateExtensionAllAndAuto(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedAgentK(t, dir, installedVersion)
	installedPresenter(t, dir)
	serveReleases(t, agentkRelease(t, "agentk-v"+next, next),
		fakeRelease{tag: "presenter-v" + next, pkg: "presenter", version: next,
			tgz: presenterTarball(t, next, "")})
	code, out, errs := run("update_extension", "--all", "--check", "--data-dir", dir)
	want(t, "--all --check", code, 0, out+errs, "AgentK "+next+" is available")
	want(t, "--all --check", code, 0, out, "Presenter "+next+" is available")
	code, out, errs = run("update_extension", "--all", "--data-dir", dir)
	want(t, "--all", code, 0, out+errs, "Installed Presenter "+next)
	want(t, "--all", code, 0, out, "Installed AgentK "+next)

	code, out, errs = run("update_extension", "presenter", "--auto=off", "--data-dir", dir)
	want(t, "--auto=off", code, 0, out+errs, "only reports Presenter's updates")
	reg, _ := extensions.Load(dir)
	if reg.Entries["presenter"].Auto() || !reg.Entries["agentk"].Auto() {
		t.Errorf("auto %+v", reg.Entries)
	}
	code, _, errs = run("update_extension", "presenter", "--auto=maybe", "--data-dir", dir)
	want(t, "--auto=maybe", code, 1, errs, "--auto takes on or off")
}

// After a scheduled check, an extension set to Update automatically is updated as an automatic job;
// one that says it's busy waits, and so does one set not to.
func TestAutoUpdate(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedPresenter(t, dir)
	serveReleases(t, fakeRelease{tag: "presenter-v" + next, pkg: "presenter", version: next,
		tgz: presenterTarball(t, next, "")})
	w := testListener(t, dir)
	u := &update.Updater{ExtensionsOnly: true, Client: newReleaseClient("o/r"), DataDir: dir}
	w.watchUpdates(u)
	if _, err := u.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.ExtensionUpdates("presenter"); got.Latest != next || !got.UpdateAvailable || !got.AutoUpdate {
		t.Fatalf("after the check: %+v", got)
	}
	state := extensions.Dir(dir, "presenter")
	if err := os.WriteFile(filepath.Join(state, "busy"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.autoUpdate(context.Background())
	if got := w.ExtensionUpdates("presenter"); app.Current() != installedVersion || got.Waiting != "in use" {
		t.Errorf("busy: current %s, %+v", app.Current(), got)
	}

	if err := setAutoUpdate(paths.New(dir), "presenter", false); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(state, "busy"))
	w.autoUpdate(context.Background())
	if got := w.ExtensionUpdates("presenter"); app.Current() != installedVersion || got.Waiting != "" {
		t.Errorf("off: current %s, %+v", app.Current(), got)
	}

	if err := setAutoUpdate(paths.New(dir), "presenter", true); err != nil {
		t.Fatal(err)
	}
	w.autoUpdate(context.Background())
	if app.Current() != next {
		t.Errorf("on: current %s", app.Current())
	}
	if j := w.jobs.Get("presenter"); j == nil || !j.Auto || j.Action != "update" || j.State != "done" {
		t.Errorf("job %+v", j)
	}
}
