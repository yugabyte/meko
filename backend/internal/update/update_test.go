package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
		rel("v0.20261030.0", "agent-k-local-0.20261030.0-linux-x64.tar.gz", Sums), // AgentK's
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
		t.Errorf("AgentK picked %q %q", r.Tag, v)
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
func tarball(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "meko-" + version + "/"
	add := func(name, body string, mode int64) {
		h := &tar.Header{Name: top + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("bin/meko", fakeMeko(version, false), 0o755)
	add("memory_engine/dist/main.js", "// memory engine\n", 0o644)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// releaseServer serves a GitHub API with one Meko release of version for this platform.
func releaseServer(t *testing.T, version string, tgz []byte, sums string) *Client {
	t.Helper()
	goos, arch := Platform()
	name := TarballName(version, goos, arch)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_ = json.NewEncoder(w).Encode([]Release{{Tag: TagPrefix + version, Assets: []Asset{
				{Name: name, URL: srv.URL + "/asset/tar"}, {Name: Sums, URL: srv.URL + "/asset/sums"},
			}}})
		case "/asset/tar":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no", http.StatusNotFound)
				return
			}
			_, _ = w.Write(tgz)
		case "/asset/sums":
			_, _ = io.WriteString(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Repo: "o/r", Token: "tok", API: srv.URL, HTTP: srv.Client()}
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
	if _, err := os.Stat(filepath.Join(a.Dir(next), "memory_engine", "dist", "main.js")); err != nil {
		t.Errorf("unpacked: %v", err)
	}
	if got, ver, ok := Of(a.CurrentExe()); !ok || ver != next || got.Root != a.Root {
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
		if err := extract(f, t.TempDir()); err == nil {
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
	if got, ver, ok := Of(link); !ok || ver != "0.20261021.0" || got.Root != a.Root {
		t.Fatalf("Of(link) = %v %q %v", got, ver, ok)
	}
	if err := a.UnlinkCommand(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("link left")
	}
}
