package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rel(tag string, assets ...string) Release {
	r := Release{Tag: tag}
	for _, a := range assets {
		r.Assets = append(r.Assets, Asset{Name: a})
	}
	return r
}

func TestPickMeko(t *testing.T) {
	tb := func(v string) string { return TarballName(v, "linux", "x64") }
	rels := []Release{
		rel("v0.20261030.0", "agent-k-local-0.20261030.0-linux-x64.tar.gz", Sums), // Kode's
		rel("agentk-v0.20261031.0", Sums),
		rel("meko-v0.20261021.1", tb("0.20261021.1"), Sums),
		rel("meko-v0.20261021.10", tb("0.20261021.10"), Sums),
		rel("meko-v0.20261022.0", tb("0.20261022.0")), // no SHA256SUMS
		rel("meko-v0.20261023.0", TarballName("0.20261023.0", "darwin", "arm64"), Sums),
		{Tag: "meko-v0.20261024.0", Prerelease: true, Assets: []Asset{{Name: tb("0.20261024.0")}, {Name: Sums}}},
		rel("meko-v0.20261025.0", tb("0.20261025.0"), Sums),
	}
	_, v, ok := PickMeko(rels, "linux", "x64", map[string]bool{"0.20261025.0": true})
	if !ok || v != "0.20261021.10" {
		t.Fatalf("picked %q %v, want 0.20261021.10 (numeric order, bad version skipped)", v, ok)
	}
	_, v, _ = PickMeko(rels, "darwin", "arm64", nil)
	if v != "0.20261023.0" {
		t.Errorf("darwin picked %q", v)
	}
	r, v, ok := Pick(rels, []string{"agentk-v", "v"}, func(string) []string { return []string{Sums} }, nil)
	if !ok || v != "0.20261031.0" || r.Tag != "agentk-v0.20261031.0" {
		t.Errorf("Kode picked %q %q", r.Tag, v)
	}
}

func TestParseVersion(t *testing.T) {
	for _, bad := range []string{"", "1.2", "1.2.3.4", "a.b.c", "01.2.3", "1.2.-3", "1.2.3-beta"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	a, _ := ParseVersion("0.20261021.9")
	b, _ := ParseVersion("0.20261021.10")
	if !a.Less(b) || b.Less(a) {
		t.Error("order")
	}
}

// fakeMeko is a release's bin/meko: `version --json` prints its version, or it fails with fail.
func fakeMeko(version string, fail bool) string {
	if fail {
		return "#!/bin/sh\necho 'cannot execute binary file' >&2\nexit 126\n"
	}
	return fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'version --json' ] && echo '{\"version\":\"%s\"}'\n", version)
}

// tarball builds meko-<version>.tar.gz holding bin/meko and an engine file.
func tarball(t *testing.T, version string) []byte { return tarballOf(t, "meko", "meko", version) }

// tarballOf builds a release of package pkg: <pkg>-<version>/bin/<name> and an engine file.
func tarballOf(t *testing.T, pkg, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := pkg + "-" + version + "/"
	add := func(file, body string, mode int64) {
		h := &tar.Header{Name: top + file, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("bin/"+name, fakeMeko(version, false), 0o755)
	add("context_engine/dist/main.js", "// context engine\n", 0o644)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// testReleasePub and testReleaseKey sign the fake releases these tests serve; their clients trust
// only it.
var testReleasePub, testReleaseKey, _ = ed25519.GenerateKey(rand.Reader)

// releaseServer serves a GitHub API with one Meko release of version for this platform, its sums
// signed by testReleaseKey.
func releaseServer(t *testing.T, version string, tgz []byte, sums string) *Client {
	t.Helper()
	return signedReleaseServer(t, version, tgz, sums, sigLine(testReleaseKey, sums))
}

// signedReleaseServer is releaseServer's release with sig as its SHA256SUMS.sig, or none when sig
// is empty.
func signedReleaseServer(t *testing.T, version string, tgz []byte, sums, sig string) *Client {
	t.Helper()
	goos, arch := Platform()
	name := TarballName(version, goos, arch)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			assets := []Asset{{Name: name, URL: srv.URL + "/asset/tar"}, {Name: Sums, URL: srv.URL + "/asset/sums"}}
			if sig != "" {
				assets = append(assets, Asset{Name: SumsSig, URL: srv.URL + "/asset/sig"})
			}
			_ = json.NewEncoder(w).Encode([]Release{{Tag: TagPrefix + version, URL: srv.URL + "/page", Assets: assets}})
		case "/asset/tar":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no", http.StatusNotFound)
				return
			}
			_, _ = w.Write(tgz)
		case "/asset/sums":
			_, _ = io.WriteString(w, sums)
		case "/asset/sig":
			_, _ = io.WriteString(w, sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Repo: "o/r", Token: "tok", API: srv.URL, HTTP: srv.Client(), Keys: []ed25519.PublicKey{testReleasePub}}
}

// realPath is path with its symlinks resolved, as Of reports it: on macOS the temp folder is under
// /var, a link to /private/var.
func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newApp(t *testing.T, versions ...string) App {
	t.Helper()
	a := App{Root: filepath.Join(t.TempDir(), "app")}
	for _, v := range versions {
		dir := a.Dir(v)
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a.Exe(v), []byte(fakeMeko(v, false)), 0o755); err != nil { //nolint:gosec // G306: test
			t.Fatal(err)
		}
		//nolint:gosec // G306: a test file
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(versions) > 0 {
		if err := a.Repoint(versions[len(versions)-1]); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func TestUpdaterStagesChecksAndSwitches(t *testing.T) {
	const old, next = "0.20261021.0", "0.20261022.0"
	a := newApp(t, old)
	goos, arch := Platform()
	tgz := tarball(t, next)
	c := releaseServer(t, next, tgz, sum(tgz)+"  "+TarballName(next, goos, arch)+"\n")
	data := t.TempDir()
	u := &Updater{App: a, Version: old, Client: c, DataDir: data, Auto: true, Schema: 5,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r, v, newer, err := u.Check(context.Background())
	if err != nil || !newer || v != next {
		t.Fatalf("check: %v %v %v", v, newer, err)
	}
	if st, ok := ReadState(data); !ok || st.Latest != next || st.Running != old {
		t.Errorf("state %+v", st)
	}
	if err := u.Apply(context.Background(), r, v); err != nil {
		t.Fatal(err)
	}
	if a.Current() != next {
		t.Fatalf("current %q", a.Current())
	}
	if _, err := os.Stat(filepath.Join(a.Dir(next), "context_engine", "dist", "main.js")); err != nil {
		t.Errorf("unpacked: %v", err)
	}
	if got, ver, ok := Of(a.CurrentExe()); !ok || ver != next || got.Root != realPath(t, a.Root) {
		t.Errorf("Of(current) = %v %q %v", got, ver, ok)
	}
	p, _ := a.ReadPending()
	if p == nil || p.From != old || p.To != next || p.Schema != 5 {
		t.Fatalf("pending %+v", p)
	}
}

func TestStageRefusesABadChecksumOrBinary(t *testing.T) {
	const next = "0.20261022.0"
	goos, arch := Platform()
	tgz := tarball(t, next)
	a := newApp(t, "0.20261021.0")
	c := releaseServer(t, next, tgz, strings.Repeat("0", 64)+"  "+TarballName(next, goos, arch)+"\n")
	rels, _ := c.Releases(context.Background())
	err := a.Stage(context.Background(), c, rels[0], next)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
	// A tarball whose binary can't run here.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := fakeMeko("0.20261023.0", true)
	_ = tw.WriteHeader(&tar.Header{Name: "meko-0.20261023.0/bin/meko", Mode: 0o755, Size: int64(len(body)),
		Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	f := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(f, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Unpack(f, "0.20261023.0"); err == nil || !strings.Contains(err.Error(), "doesn't run on this machine") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(a.Dir("0.20261023.0")); !os.IsNotExist(err) {
		t.Error("a binary that can't run was installed")
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	for _, h := range []*tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg},
		{Name: "meko-1.2.3/link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"},
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(h)
		_ = tw.Close()
		_ = gz.Close()
		f := filepath.Join(t.TempDir(), "x.tar.gz")
		if err := os.WriteFile(f, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := extract(f, t.TempDir(), false); err == nil {
			t.Errorf("%s was extracted", h.Name)
		}
	}
}

func TestRollBackAfterThreeFailedStarts(t *testing.T) {
	const old, next = "0.20261021.0", "0.20261022.0"
	a := newApp(t, old, next)
	if err := a.Switch(old, next, 4); err != nil {
		t.Fatal(err)
	}
	// Three starts that crash, and one clean stop that doesn't count.
	for i := 0; i < MaxFailedStarts; i++ {
		if p, err := a.Started(next); err != nil || p != nil {
			t.Fatalf("start %d: rollback %v, %v", i+1, p, err)
		}
		if i == 1 {
			if p, _ := a.Started(next); p != nil {
				t.Fatal("rolled back early")
			}
			if err := a.Uncount(next); err != nil {
				t.Fatal(err)
			}
		}
	}
	p, err := a.Started(next)
	if err != nil || p == nil || p.From != old || p.Schema != 4 {
		t.Fatalf("4th start: %+v, %v", p, err)
	}
	if err := a.RollBack(*p); err != nil {
		t.Fatal(err)
	}
	if a.Current() != old || !a.BadVersions()[next] {
		t.Fatalf("current %q, bad %v", a.Current(), a.BadVersions())
	}
	if p, _ := a.ReadPending(); p != nil {
		t.Errorf("pending left: %+v", p)
	}
}

func TestHealthyClearsPendingAndPrunes(t *testing.T) {
	a := newApp(t, "0.20261020.0", "0.20261021.0", "0.20261022.0")
	if err := a.Switch("0.20261021.0", "0.20261022.0", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Started("0.20261022.0"); err != nil {
		t.Fatal(err)
	}
	gone, err := a.Healthy("0.20261022.0")
	if err != nil || strings.Join(gone, ",") != "0.20261020.0" {
		t.Fatalf("pruned %v, %v", gone, err)
	}
	if strings.Join(a.Versions(), ",") != "0.20261021.0,0.20261022.0" {
		t.Errorf("left %v", a.Versions())
	}
	if p, _ := a.ReadPending(); p != nil {
		t.Errorf("pending left: %+v", p)
	}
}

func TestStartedByTheOldVersionClearsPending(t *testing.T) {
	a := newApp(t, "0.20261021.0", "0.20261022.0")
	if err := a.Switch("0.20261021.0", "0.20261022.0", 4); err != nil {
		t.Fatal(err)
	}
	if p, err := a.Started("0.20261021.0"); err != nil || p != nil {
		t.Fatal(p, err)
	}
	if p, _ := a.ReadPending(); p != nil {
		t.Errorf("pending left: %+v", p)
	}
}

func TestLinkCommand(t *testing.T) {
	a := newApp(t, "0.20261021.0")
	home := t.TempDir()
	link, err := a.LinkCommand(home)
	if err != nil {
		t.Fatal(err)
	}
	if got, ver, ok := Of(link); !ok || ver != "0.20261021.0" || got.Root != realPath(t, a.Root) {
		t.Fatalf("Of(link) = %v %q %v", got, ver, ok)
	}
	if err := a.UnlinkCommand(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("link left")
	}
}

// An extension's releases (Presenter's) go through the same App: its tarball and top folder named for
// its package (present), its binary and its link in ~/.local/bin for its program (me_present).
func TestNamedAppStagesAnExtensionsRelease(t *testing.T) {
	const next = "0.20261024.0"
	goos, arch := Platform()
	name := PackageTarball("present", next, goos, arch)
	if want := "present-" + next + "-" + goos + "-" + arch + ".tar.gz"; name != want {
		t.Fatalf("PackageTarball = %q, want %q", name, want)
	}
	tgz := tarballOf(t, "present", "me_present", next)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]Release{
				{Tag: TagPrefix + "0.20261030.0", Assets: []Asset{{Name: TarballName("0.20261030.0", goos, arch)}, {Name: Sums}}},
				{Tag: "present-v0.20261025.0", Assets: []Asset{{Name: Sums}}}, // no tarball for this platform
				{Tag: "present-v" + next, Assets: []Asset{
					{Name: name, BrowserURL: srv.URL + "/tar"}, {Name: Sums, BrowserURL: srv.URL + "/sums"},
					{Name: SumsSig, BrowserURL: srv.URL + "/sig"},
				}},
			})
		case "/tar":
			_, _ = w.Write(tgz)
		case "/sums":
			_, _ = io.WriteString(w, sum(tgz)+"  "+name+"\n")
		case "/sig":
			_, _ = io.WriteString(w, sigLine(testReleaseKey, sum(tgz)+"  "+name+"\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{Repo: "o/r", API: srv.URL, HTTP: srv.Client(), Keys: []ed25519.PublicKey{testReleasePub}}
	rels, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, v, ok := PickPackage(rels, []string{"present-v"}, []string{"present"}, goos, arch, nil)
	if !ok || v != next {
		t.Fatalf("picked %q %v", v, ok)
	}
	bad := map[string]bool{next: true}
	if _, _, ok := PickPackage(rels, []string{"present-v"}, []string{"present"}, goos, arch, bad); ok {
		t.Error("a bad version was picked")
	}
	a := App{Root: filepath.Join(t.TempDir(), "app"), Name: "me_present", Package: "present"}
	if err = a.Stage(context.Background(), c, r, v); err != nil {
		t.Fatal(err)
	}
	if err = a.Repoint(v); err != nil {
		t.Fatal(err)
	}
	if a.Exe(v) != filepath.Join(a.Root, v, "bin", "me_present") {
		t.Errorf("Exe = %s", a.Exe(v))
	}
	home := t.TempDir()
	link, err := a.LinkCommand(home)
	if err != nil || link != filepath.Join(home, ".local", "bin", "me_present") {
		t.Fatalf("link %q %v", link, err)
	}
	if got, ver, ok := Of(link); !ok || ver != next || got.Root != realPath(t, a.Root) {
		t.Errorf("Of(link) = %v %q %v", got, ver, ok)
	}
}

// A release from before its program's rename (only bin/me_kode's old name, me_koder) gets the
// new name as a link when it's unpacked, and a version already installed gets it when app/current is
// pointed at it, or when a failed update rolls back to it: Exe, CurrentExe and the link in
// ~/.local/bin run it whichever version is current.
func TestAppAdoptsAReleaseFromBeforeItsRename(t *testing.T) {
	const old, older = "0.20261024.0", "0.20261023.0"
	a := App{Root: filepath.Join(t.TempDir(), "app"), Name: "me_kode", OldNames: []string{"me_koder"},
		Package: "agentk", Private: true}
	if err := os.MkdirAll(a.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(f, tarballOf(t, "agentk", "me_koder", old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Unpack(f, old); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(a.Exe(old)); err != nil || target != "me_koder" {
		t.Errorf("unpacked: bin/me_kode -> %q (%v)", target, err)
	}
	// Unpacked by a Meko from before the rename: only the old name.
	bin := filepath.Join(a.Dir(older), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's program
	if err := os.WriteFile(filepath.Join(bin, "me_koder"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Repoint(older); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.CurrentExe()); err != nil {
		t.Errorf("app/current/bin/me_kode: %v", err)
	}
	// Adopting again changes nothing, and a version with neither name is left alone.
	if err := a.AdoptAll(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.Dir("0.20261022.0"), "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Adopt("0.20261022.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.Exe("0.20261022.0")); !os.IsNotExist(err) {
		t.Errorf("a version with no program got a link: %v", err)
	}
	// An update from a version nothing has adopted yet (Meko's start hasn't run since the rename) to
	// one that keeps failing still rolls back to it: Started adopts the version it returns to.
	const unadopted = "0.20261021.0"
	ubin := filepath.Join(a.Dir(unadopted), "bin")
	if err := os.MkdirAll(ubin, 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test's program
	if err := os.WriteFile(filepath.Join(ubin, "me_koder"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Switch(unadopted, old, 4); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxFailedStarts; i++ {
		if p, err := a.Started(old); err != nil || p != nil {
			t.Fatalf("start %d: rollback %v, %v", i+1, p, err)
		}
	}
	if p, err := a.Started(old); err != nil || p == nil || p.From != unadopted {
		t.Fatalf("no rollback to the version from before the rename: %+v, %v", p, err)
	}
}

// A private app folder (an extension's, in Meko's data folder) gets its release owner-only:
// folders and programs 0700, other files 0600. Meko's own keeps the tarball's modes.
func TestPrivateAppUnpacksOwnerOnly(t *testing.T) {
	const v = "0.20261024.0"
	f := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(f, tarballOf(t, "agentk", "me_kode", v), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, private := range []bool{true, false} {
		a := App{Root: filepath.Join(t.TempDir(), "app"), Name: "me_kode", Package: "agentk", Private: private}
		if err := os.MkdirAll(a.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := a.Unpack(f, v); err != nil {
			t.Fatal(err)
		}
		want := map[string]os.FileMode{"bin": 0o755, "bin/me_kode": 0o755, "VERSION": 0o644,
			"context_engine/dist/main.js": 0o644}
		if private {
			want = map[string]os.FileMode{"bin": 0o700, "bin/me_kode": 0o700, "VERSION": 0o600,
				"context_engine/dist/main.js": 0o600}
		}
		for p, mode := range want {
			fi, err := os.Stat(filepath.Join(a.Dir(v), p))
			if err != nil || fi.Mode().Perm() != mode {
				t.Errorf("private %v: %s is %v (%v), want %v", private, p, fi.Mode().Perm(), err, mode)
			}
		}
	}
}

// webUpdater is an Updater as serve builds it, running webOld, against a release server offering
// next; restarted is closed when it asks serve to restart.
func webUpdater(t *testing.T, next, sums string) (*Updater, chan struct{}) {
	t.Helper()
	tgz := tarball(t, next)
	if sums == "" {
		goos, arch := Platform()
		sums = sum(tgz) + "  " + TarballName(next, goos, arch) + "\n"
	}
	restarted := make(chan struct{})
	u := &Updater{App: newApp(t, webOld), Version: webOld, Client: releaseServer(t, next, tgz, sums),
		DataDir: t.TempDir(), Schema: 5, Channel: ChannelStable, Context: context.Background(),
		Log: slog.New(slog.DiscardHandler), Restart: func() { close(restarted) }}
	return u, restarted
}

// webOld is the version webUpdater runs.
const webOld = "0.20261021.0"

func waitJob(t *testing.T, u *Updater) Job {
	t.Helper()
	for range 500 {
		if j := u.Status().Job; j != nil && j.State != JobRunning {
			return *j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the job didn't end: %+v", u.Status().Job)
	return Job{}
}

// The web UI's install checks, stages and switches, then asks for the restart, holding the lock
// until then; a second one is refused while the new version is pending.
func TestStartApplyInstallsAndRestarts(t *testing.T) {
	const next = "0.20261022.0"
	u, restarted := webUpdater(t, next, "")
	st, err := u.StartApply()
	if err != nil || st.Job == nil || st.Job.State != JobRunning {
		t.Fatalf("start: %+v %v", st.Job, err)
	}
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("no restart")
	}
	st = u.Status()
	if j := st.Job; j.State != JobRunning || j.Step != StepRestarting || j.To != next {
		t.Errorf("job %+v", j)
	}
	if u.App.Current() != next || st.Pending == nil || st.Pending.To != next || !st.Available ||
		st.Latest != next || !strings.HasSuffix(st.LatestURL, "/page") {
		t.Errorf("current %q, status %+v", u.App.Current(), st)
	}
	if _, err := u.StartApply(); err != ErrPending {
		t.Errorf("a second install: %v", err)
	}
	if _, err := u.CheckNow(context.Background()); err != ErrBusy {
		t.Errorf("a check before the restart: %v", err)
	}
}

// Nothing newer ends the job done with nothing installed; a failed install ends it failed. Either
// way the lock is free again.
func TestStartApplyEnds(t *testing.T) {
	const v = webOld
	u, _ := webUpdater(t, v, "")
	if _, err := u.StartApply(); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, u); j.State != JobDone || j.To != "" || j.Error != "" || j.FinishedAt == nil {
		t.Errorf("nothing newer: %+v", j)
	}
	if _, err := u.CheckNow(context.Background()); err != nil {
		t.Errorf("check after: %v", err)
	}

	const next = "0.20261022.0"
	goos, arch := Platform()
	u, restarted := webUpdater(t, next, strings.Repeat("0", 64)+"  "+TarballName(next, goos, arch)+"\n")
	if _, err := u.StartApply(); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, u)
	if j.State != JobFailed || j.To != next || !strings.Contains(j.Error, "checksum mismatch") {
		t.Errorf("bad checksum: %+v", j)
	}
	if u.App.Current() != v {
		t.Errorf("current %q", u.App.Current())
	}
	select {
	case <-restarted:
		t.Error("restarted after a failed install")
	default:
	}
	if _, err := u.StartApply(); err != nil {
		t.Errorf("again after a failure: %v", err)
	}
	waitJob(t, u)
}

// While a check or an install holds the lock, the web UI's are refused and the scheduled ones skip
// their turn.
func TestUpdaterRefusesWhileBusy(t *testing.T) {
	u, _ := webUpdater(t, "0.20261022.0", "")
	u.lock().held.Lock()
	if _, err := u.CheckNow(context.Background()); err != ErrBusy {
		t.Errorf("check: %v", err)
	}
	if _, err := u.StartApply(); err != ErrBusy {
		t.Errorf("install: %v", err)
	}
	u.FirstCheck, u.Interval = time.Millisecond, time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	u.Run(ctx)
	if _, ok := ReadState(u.DataDir); ok {
		t.Error("a scheduled check ran while busy")
	}
	u.lock().held.Unlock()
	st, err := u.CheckNow(context.Background())
	if err != nil || st.CheckedAt == nil || st.Latest != "0.20261022.0" || !st.Available {
		t.Errorf("check: %+v %v", st, err)
	}
}

// Status lists the rolled-back versions, sorted, and compares the latest with the running version.
func TestStatus(t *testing.T) {
	u, _ := webUpdater(t, "0.20261022.0", "")
	st := u.Status()
	if !st.Release || st.Running != "0.20261021.0" || st.Channel != ChannelStable || st.CheckedAt != nil ||
		st.Available || len(st.RolledBack) != 0 || st.Job != nil {
		t.Errorf("before a check: %+v", st)
	}
	for _, v := range []string{"0.20261020.2", "0.20261019.0"} {
		if err := u.App.addBadVersion(v); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(State{CheckedAt: time.Now(), Running: "0.20261020.0", Latest: "0.20261021.0"})
	if err := os.WriteFile(StateFile(u.DataDir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st = u.Status()
	if strings.Join(st.RolledBack, ",") != "0.20261019.0,0.20261020.2" || st.Available || st.Latest != "0.20261021.0" {
		t.Errorf("after: %+v", st)
	}

	// A check from before a rollback still names the version rolled back: it isn't offered.
	b, _ = json.Marshal(State{CheckedAt: time.Now(), Running: webOld, Latest: "0.20261022.0"})
	if err := os.WriteFile(StateFile(u.DataDir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if st = u.Status(); !st.Available {
		t.Errorf("newer: %+v", st)
	}
	if err := u.App.addBadVersion("0.20261022.0"); err != nil {
		t.Fatal(err)
	}
	if st = u.Status(); st.Available || st.Latest != "0.20261022.0" {
		t.Errorf("rolled back: %+v", st)
	}
}

// Off refuses checks and installs, saying why.
func TestOff(t *testing.T) {
	o := Off{Version: webOld, Reason: "no schema"}
	if st := o.Status(); !st.Release || st.Off != "no schema" || st.Running != webOld || st.RolledBack == nil {
		t.Errorf("status %+v", st)
	}
	if _, err := o.CheckNow(context.Background()); err != ErrOff {
		t.Errorf("check: %v", err)
	}
	if _, err := o.StartApply(); err != ErrOff {
		t.Errorf("install: %v", err)
	}
}

func TestTokenlessRefusalsSayWhy(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		token   string
		want    string
	}{
		{"rate limit", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1793300000"}, "",
			"60 calls an hour without a token and they're spent: try again after "},
		{"too many requests", 429, nil, "", "try again within the hour, or sign in with gh auth login"},
		{"private repo", 404, nil, "", "If the repo is private, sign in with gh auth login"},
		{"forbidden", 403, map[string]string{"X-RateLimit-Remaining": "59"}, "", "If the repo is private"},
		// The call that spends the last of the 60 gets its usual answer, with the header at 0.
		{"private repo on the last call", 404, map[string]string{"X-RateLimit-Remaining": "0"}, "",
			"If the repo is private, sign in with gh auth login"},
		{"with a token", 404, nil, "tok", ""},
		{"server error", 502, nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			c := &Client{Repo: "o/r", Token: tc.token, API: srv.URL, HTTP: srv.Client()}
			_, err := c.Releases(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			msg := err.Error()
			if tc.want == "" {
				if strings.Contains(msg, "sign in") || strings.Contains(msg, "private") {
					t.Errorf("no hint wanted: %s", msg)
				}
			} else if !strings.Contains(msg, tc.want) {
				t.Errorf("want %q in: %s", tc.want, msg)
			}
			if strings.HasSuffix(msg, ".") {
				t.Errorf("ends with a period: %s", msg)
			}
		})
	}
}

// A package renamed (Kode's agentk) is found by either name: the newest release by its new tarball,
// an older one asked for by version by its old one.
func TestPickPackageByAnOldName(t *testing.T) {
	goos, arch := "linux", "x64"
	rels := []Release{
		rel("kode-v0.20261107.0", PackageTarball("kode", "0.20261107.0", goos, arch), Sums),
		rel("agentk-v0.20261101.0", PackageTarball("agentk", "0.20261101.0", goos, arch), Sums),
	}
	names, prefixes := []string{"kode", "agentk"}, []string{"kode-v", "agentk-v", "v"}
	r, v, ok := PickPackage(rels, prefixes, names, goos, arch, nil)
	if !ok || v != "0.20261107.0" || r.Tag != "kode-v0.20261107.0" {
		t.Errorf("newest: %q %q %v", r.Tag, v, ok)
	}
	if r, ok := PickPackageVersion(rels, prefixes, names, "0.20261101.0", goos, arch); !ok ||
		r.Tag != "agentk-v0.20261101.0" {
		t.Errorf("an older one: %q %v", r.Tag, ok)
	}
	if _, ok := PickPackageVersion(rels, prefixes, []string{"kode"}, "0.20261101.0", goos, arch); ok {
		t.Error("found by a name its tarball doesn't have")
	}
}
