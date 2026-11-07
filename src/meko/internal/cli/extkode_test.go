package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// fakeAgentK is a Kode release's bin/me_kode, keeping its state in its folder in the data folder
// (MEKO_HOME, which Meko sets): install, start, stop and restart log to its log with its version,
// status succeeds while it runs, and selftest passes. With rollback-to there, restart does what
// meko _launch does after three failed starts: points app/current back at that version. uninstall
// logs to agentk-uninstall.log beside its folder, and with --purge deletes its state.db.
func fakeAgentK(version string) string {
	return `#!/bin/sh
s="$MEKO_HOME/extensions/kode"
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
  uninstall) echo "$*" >> "$MEKO_HOME/agentk-uninstall.log"; rm -f "$s/running"
    [ "$2" != --purge ] || rm -f "$s/state.db" ;;
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

// fakeRelease is a release to serve: its tag, and the package and version of the tarball it carries,
// with its SHA256SUMS signed by testReleaseKey unless unsigned.
type fakeRelease struct {
	tag, pkg, version string
	tgz               []byte
	unsigned          bool
}

// testReleasePub and testReleaseKey sign the releases the CLI's tests serve; their clients trust
// only it.
var testReleasePub, testReleaseKey = func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub, k
}()

// testClient is a release client for the fake GitHub API srv, trusting testReleaseKey.
func testClient(srv *httptest.Server) *update.Client {
	return &update.Client{Repo: "o/r", API: srv.URL, HTTP: srv.Client(), Keys: []ed25519.PublicKey{testReleasePub}}
}

// signSums is testReleaseKey's SHA256SUMS.sig for sums, as the release workflows write it.
func signSums(sums string) string {
	return update.KeyID(testReleasePub) + " " +
		base64.StdEncoding.EncodeToString(ed25519.Sign(testReleaseKey, []byte(sums))) + "\n"
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
				assets := []update.Asset{
					{Name: update.PackageTarball(rel.pkg, rel.version, goos, arch), BrowserURL: fmt.Sprintf("%s/tar/%d", srv.URL, i)},
					{Name: update.Sums, BrowserURL: fmt.Sprintf("%s/sums/%d", srv.URL, i)},
				}
				if !rel.unsigned {
					assets = append(assets, update.Asset{Name: update.SumsSig, BrowserURL: fmt.Sprintf("%s/sig/%d", srv.URL, i)})
				}
				list = append(list, update.Release{Tag: rel.tag, URL: srv.URL + "/page/" + rel.tag, Assets: assets})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/tar/%d", &i); err == nil && i < len(rels) {
			_, _ = w.Write(rels[i].tgz)
			return
		}
		sums := func(i int) string {
			h := sha256.Sum256(rels[i].tgz)
			return fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]),
				update.PackageTarball(rels[i].pkg, rels[i].version, goos, arch))
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/sums/%d", &i); err == nil && i < len(rels) {
			_, _ = fmt.Fprint(w, sums(i))
			return
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/sig/%d", &i); err == nil && i < len(rels) {
			_, _ = fmt.Fprint(w, signSums(sums(i)))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	old := newReleaseClient
	newReleaseClient = func(string) *update.Client { return testClient(srv) }
	t.Cleanup(func() { newReleaseClient = old })
}

func kodeRelease(t *testing.T, tag, version string) fakeRelease {
	return kodeReleaseAs(t, tag, version, "me_kode")
}

// kodeReleaseAs is a release of Kode whose program is named command: agentk before me_kode.
func kodeReleaseAs(t *testing.T, tag, version, command string) fakeRelease {
	return releaseOf(t, "kode", tag, version, command)
}

// agentkRelease is a release of Kode from before Meko called it kode: its tarball is agentk's.
func agentkRelease(t *testing.T, tag, version, command string) fakeRelease {
	return releaseOf(t, "agentk", tag, version, command)
}

func releaseOf(t *testing.T, pkg, tag, version, command string) fakeRelease {
	return fakeRelease{tag: tag, pkg: pkg, version: version,
		tgz: packageTarball(t, pkg, command, version, fakeAgentK(version))}
}

// installedAgentK is a release install of Kode's version, as extension install leaves it.
func installedAgentK(t *testing.T, dir, version string) update.App {
	t.Helper()
	app := extensions.Known["kode"].App(dir, "kode")
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
	reg.Entries["kode"] = extensions.Entry{Command: link, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	return app
}

// agentkFromBeforeTheRename installs Kode as Meko did while its command was agentk: a version with
// only bin/agentk, ~/.local/bin/agentk linking to it, and the registry running that link, which it
// returns.
func agentkFromBeforeTheRename(t *testing.T, dir string) string {
	t.Helper()
	const version = "0.20261030.0"
	app := extensions.Known["kode"].App(dir, "kode")
	bin := filepath.Join(app.Dir(version), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's fake program
	if err := os.WriteFile(filepath.Join(bin, "agentk"), []byte(fakeAgentK(version)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Dir(version), "VERSION"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(version, filepath.Join(app.Root, "current")); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	old := filepath.Join(home, ".local", "bin", "agentk")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(app.Root, "current", "bin", "agentk"), old); err != nil {
		t.Fatal(err)
	}
	reg, _ := extensions.Load(dir)
	reg.Entries["kode"] = extensions.Entry{Command: old, InstalledByMeko: true}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	return old
}

// wantRenamed checks that Kode installed by agentkFromBeforeTheRename runs as me_kode, with no
// command on PATH: the registry runs app/current/bin/me_kode, no ~/.local/bin/me_kode is made, and
// the old link is gone.
func wantRenamed(t *testing.T, dir, old string) {
	t.Helper()
	exe := extensions.Known["kode"].App(dir, "kode").CurrentExe()
	if _, err := os.Stat(exe); err != nil {
		t.Errorf("app/current/bin/me_kode: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(old), "me_kode")); !os.IsNotExist(err) {
		t.Errorf("~/.local/bin/me_kode was made: %v", err)
	}
	if reg, _ := extensions.Load(dir); reg.Entries["kode"].Command != exe {
		t.Errorf("registry %+v", reg.Entries["kode"])
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Errorf("~/.local/bin/agentk is left: %v", err)
	}
}

// Meko's start moves a Kode installed while its command was agentk onto me_kode: an update never
// runs its install, so this is where ~/.local/bin/me_kode comes from, and ~/.local/bin/agentk goes.
func TestStartMovesAgentKOntoItsNewCommand(t *testing.T) {
	dir, port := setup(t)
	noRealExtensions(t)
	old := agentkFromBeforeTheRename(t, dir)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	wantRenamed(t, dir, old)
}

// extension install kode installs Kode's newest release as it does Presenter's (no installer of
// Kode's), then runs its install with this Meko's URL and key and what followed --. --version
// installs another release, under either tag prefix: one from before the program was me_kode gets
// that name too. The links of its old names into this data folder go.
func TestInstallExtensionInstallsAgentKsRelease(t *testing.T) {
	const newest, older = "0.20261030.0", "0.20261029.0"
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	serveReleases(t, kodeRelease(t, "kode-v"+newest, newest), agentkRelease(t, "v"+older, older, "agentk"))
	app := extensions.Known["kode"].App(dir, "kode")
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// An install from before the rename left these.
	for _, n := range []string{"agentk", "agent-k-local"} {
		if err := os.Symlink(filepath.Join(app.Root, "current", "bin", n), filepath.Join(bin, n)); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs = run("extension", "install", "kode", "--data-dir", dir, "--", "--agent", "kiro")
	want(t, "extension install", code, 0, out+errs,
		"  ✓ Release         Kode "+newest+", downloaded and checked")
	if app.Current() != newest {
		t.Errorf("current %q", app.Current())
	}
	reg, _ := extensions.Load(dir)
	if reg.Entries["kode"].Command != app.CurrentExe() {
		t.Errorf("registry %+v", reg.Entries)
	}
	for _, n := range []string{"me_kode", "agentk", "agent-k-local"} {
		if _, err := os.Lstat(filepath.Join(bin, n)); !os.IsNotExist(err) {
			t.Errorf("~/.local/bin/%s is left: %v", n, err)
		}
	}
	_, tok, _ := run("token", "--data-dir", dir)
	log := filepath.Join(extensions.Dir(dir, "kode"), "log")
	wantArgs := "install --meko-url http://127.0.0.1:" + port + "/mcp --meko-key " + strings.TrimSpace(tok) +
		" --non-interactive --agent kiro\n"
	if got := readLog(t, log); got != wantArgs {
		t.Errorf("agentk got %q, want %q", got, wantArgs)
	}
	code, out, errs = run("extension", "install", "kode", "--version", older, "--data-dir", dir)
	want(t, "extension install --version", code, 0, out+errs,
		"  ✓ Release         Kode "+older+", downloaded and checked")
	if app.Current() != older {
		t.Errorf("current %q after --version", app.Current())
	}
	if _, err := os.Stat(app.CurrentExe()); err != nil {
		t.Errorf("app/current/bin/me_kode doesn't run the release from before the rename: %v", err)
	}
	code, _, errs = run("extension", "install", "kode", "--version", "0.20261001.0", "--data-dir", dir)
	want(t, "a version with no release", code, 1, errs, "no release 0.20261001.0 of Kode")
	// Going back to a version keeps Update automatically off, so the next check doesn't undo it.
	code, out, errs = run("extension", "update", "kode", "--auto=off", "--data-dir", dir)
	want(t, "--auto=off", code, 0, out+errs, "only reports Kode's updates")
	code, out, errs = run("extension", "install", "kode", "--version", newest, "--data-dir", dir)
	want(t, "extension install --version again", code, 0, out+errs, "  ✓ Release         Kode "+newest)
	if reg, _ = extensions.Load(dir); reg.Entries["kode"].Auto() {
		t.Errorf("the switch came back on: %+v", reg.Entries["kode"])
	}
}

// A release with no SHA256SUMS.sig isn't installed by default, and the error names the setting that
// installs it anyway; with MEKO_UPDATE_REQUIRE_SIGNATURE=false it installs, with a warning row.
func TestInstallExtensionRequiresASignature(t *testing.T) {
	const v = "0.20261030.0"
	dir, port := setup(t)
	noRealExtensions(t)
	code, out, errs := run("start", "--data-dir", dir, "--port", port)
	want(t, "start", code, 0, out+errs, "is running")
	rel := kodeRelease(t, "kode-v"+v, v)
	rel.unsigned = true
	serveReleases(t, rel)
	code, out, errs = run("extension", "install", "kode", "--data-dir", dir)
	want(t, "unsigned", code, 1, out+errs,
		"kode-v"+v+" isn't signed (it has no SHA256SUMS.sig): [update] require_signature = false installs it anyway")
	app := extensions.Known["kode"].App(dir, "kode")
	if app.Current() != "" {
		t.Fatalf("an unsigned release was installed: %q", app.Current())
	}
	t.Setenv("MEKO_UPDATE_REQUIRE_SIGNATURE", "false")
	code, out, errs = run("extension", "install", "kode", "--data-dir", dir)
	want(t, "unsigned, not required", code, 0, out+errs, "  ! Release         kode-v"+v+
		" isn't signed (it has no SHA256SUMS.sig): installing it without that check, "+
		"as [update] require_signature = false says\n")
	if app.Current() != v {
		t.Errorf("current %q", app.Current())
	}
}

// An update of a running Kode switches while it runs and restarts it with me_kode restart, never
// stop and start (which would end its sessions), after checking the release with its selftest.
func TestUpdateExtensionRestartsAgentK(t *testing.T) {
	const old, next = "0.20261024.0", "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedAgentK(t, dir, old)
	state := extensions.Dir(dir, "kode")
	if err := os.WriteFile(filepath.Join(state, "running"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	serveReleases(t, kodeRelease(t, "kode-v"+next, next))
	code, out, errs := run("extension", "update", "kode", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Kode is running "+next)
	if got := readLog(t, filepath.Join(state, "log")); got != "restart "+next+"\n" {
		t.Errorf("log %q", got)
	}
	if p, _ := app.ReadPending(); app.Current() != next || p == nil || p.From != old || p.To != next {
		t.Errorf("current %q, pending %+v", app.Current(), p)
	}
}

// A stopped Kode (its status exits non-zero) is switched to the new version and left stopped: an
// update doesn't start it.
func TestUpdateExtensionLeavesAStoppedAgentKStopped(t *testing.T) {
	const old, next = "0.20261024.0", "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	app := installedAgentK(t, dir, old)
	serveReleases(t, kodeRelease(t, "kode-v"+next, next))
	code, out, errs := run("extension", "update", "kode", "--data-dir", dir)
	want(t, "extension update", code, 0, out+errs, "Installed Kode "+next+"; it runs at its next start")
	if got := readLog(t, filepath.Join(extensions.Dir(dir, "kode"), "log")); got != "" {
		t.Errorf("log %q", got)
	}
	if app.Current() != next {
		t.Errorf("current %q", app.Current())
	}
}

// When meko _launch rolls the new version back while me_kode restart waits, the update says so.
func TestUpdateExtensionReportsAgentKsRollBack(t *testing.T) {
	const old, next = "0.20261023.0", "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedAgentK(t, dir, old)
	state := extensions.Dir(dir, "kode")
	for f, body := range map[string]string{"running": "", "rollback-to": old} {
		if err := os.WriteFile(filepath.Join(state, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	serveReleases(t, kodeRelease(t, "kode-v"+next, next))
	code, _, errs := run("extension", "update", "kode", "--data-dir", dir)
	want(t, "extension update", code, 1, errs, "rolled back to "+old+" and won't be offered again")
}

// A Kode still in its old home isn't one Meko updates until it has moved.
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
	reg.Entries["kode"] = extensions.Entry{Command: exe}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run("extension", "update", "kode", "--data-dir", dir)
	want(t, "an unmoved Kode", code, 1, errs, "once it has moved into Meko's data folder")
	if u := testListener(t, dir).ExtensionUpdates("kode"); u.Updatable ||
		!strings.Contains(u.NotUpdatable, "once it has moved into Meko's data folder") {
		t.Errorf("ExtensionUpdates = %+v", u)
	}
}

// Kode's own [update] channel = "off" turns Update automatically off, saved, so it stays off once
// Kode's config no longer says so.
func TestAgentKsOldUpdatesOffCarriesOver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedAgentK(t, dir, installedVersion)
	cfg := filepath.Join(extensions.Dir(dir, "kode"), "config.toml")
	if err := os.WriteFile(cfg, []byte("[update]\nchannel = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := testListener(t, dir)
	if u := w.ExtensionUpdates("kode"); u.AutoUpdate {
		t.Errorf("on with Kode's updates off: %+v", u)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if reg, _ := extensions.Load(dir); reg.Entries["kode"].Auto() || w.ExtensionUpdates("kode").AutoUpdate {
		t.Errorf("came back on: %+v", reg.Entries["kode"])
	}
}

// extension update --all goes through every installed extension; --auto sets Update automatically.
func TestUpdateExtensionAllAndAuto(t *testing.T) {
	const next = "0.20261025.0"
	t.Setenv("HOME", t.TempDir())
	noRealExtensions(t)
	dir := shortDir(t)
	installedAgentK(t, dir, installedVersion)
	installedPresenter(t, dir)
	serveReleases(t, kodeRelease(t, "kode-v"+next, next),
		fakeRelease{tag: "present-v" + next, pkg: "present", version: next,
			tgz: presentTarball(t, next, "")})
	code, out, errs := run("extension", "update", "--all", "--check", "--data-dir", dir)
	want(t, "--all --check", code, 0, out+errs, "Kode "+next+" is available")
	want(t, "--all --check", code, 0, out, "Presenter "+next+" is available")
	code, out, errs = run("extension", "update", "--all", "--data-dir", dir)
	want(t, "--all", code, 0, out+errs, "Installed Presenter "+next)
	want(t, "--all", code, 0, out, "Installed Kode "+next)

	code, out, errs = run("extension", "update", "present", "--auto=off", "--data-dir", dir)
	want(t, "--auto=off", code, 0, out+errs, "only reports Presenter's updates")
	reg, _ := extensions.Load(dir)
	if reg.Entries["present"].Auto() || !reg.Entries["kode"].Auto() {
		t.Errorf("auto %+v", reg.Entries)
	}
	code, _, errs = run("extension", "update", "present", "--auto=maybe", "--data-dir", dir)
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
	serveReleases(t, fakeRelease{tag: "present-v" + next, pkg: "present", version: next,
		tgz: presentTarball(t, next, "")})
	w := testListener(t, dir)
	u := &update.Updater{ExtensionsOnly: true, Client: newReleaseClient("o/r"), DataDir: dir}
	w.watchUpdates(u)
	if _, err := u.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.ExtensionUpdates("present"); got.Latest != next || !got.UpdateAvailable || !got.AutoUpdate {
		t.Fatalf("after the check: %+v", got)
	}
	state := extensions.Dir(dir, "present")
	if err := os.WriteFile(filepath.Join(state, "busy"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.autoUpdate(context.Background())
	if got := w.ExtensionUpdates("present"); app.Current() != installedVersion || got.Waiting != "in use" {
		t.Errorf("busy: current %s, %+v", app.Current(), got)
	}

	if err := setAutoUpdate(paths.New(dir), "present", false); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(state, "busy"))
	w.autoUpdate(context.Background())
	if got := w.ExtensionUpdates("present"); app.Current() != installedVersion || got.Waiting != "" {
		t.Errorf("off: current %s, %+v", app.Current(), got)
	}

	if err := setAutoUpdate(paths.New(dir), "present", true); err != nil {
		t.Fatal(err)
	}
	w.autoUpdate(context.Background())
	if app.Current() != next {
		t.Errorf("on: current %s", app.Current())
	}
	if j := w.jobs.Get("present"); j == nil || !j.Auto || j.Action != "update" || j.State != "done" {
		t.Errorf("job %+v", j)
	}
}
