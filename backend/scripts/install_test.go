// Package scripts tests the release scripts in this folder.
package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const version = "0.20261022.3"

// fakeGh answers the gh calls install.sh makes from a folder of release assets.
const fakeGh = `#!/bin/sh
case "$1 $2" in
  "auth status") [ -z "$FAKE_GH_SIGNED_OUT" ] ;;
  "api "*) echo "$FAKE_TAG" ;;
  "release download")
    tag=$3; shift 3
    dir=""; pats=""
    while [ $# -gt 0 ]; do
      case $1 in --dir) dir=$2; shift 2 ;; --pattern) pats="$pats $2"; shift 2 ;; *) shift ;; esac
    done
    [ "$tag" = "$FAKE_TAG" ] || exit 1
    for p in $pats; do cp "$FAKE_ASSETS/$p" "$dir/" || exit 1; done ;;
  *) echo "fake gh: $*" >&2; exit 1 ;;
esac
`

// fakeCurl answers install.sh's curl calls without gh: the latest release's redirect and the
// asset downloads, logging each call's arguments. FAKE_CURL_404 answers 404, as GitHub does
// for an internal repo without a token.
const fakeCurl = `#!/bin/sh
echo "$*" >> "$FAKE_CURL_LOG"
[ -z "$FAKE_CURL_404" ] || { echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; }
out=""; write=""; url=""
while [ $# -gt 0 ]; do
  case $1 in -o) out=$2; shift 2 ;; -w) write=$2; shift 2 ;; -H) shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac
done
case $url in
  */releases/latest) [ "$write" = '%{url_effective}' ] && printf '%s' "${url%/latest}/tag/$FAKE_TAG" ;;
  */releases/download/"$FAKE_TAG"/*) cp "$FAKE_ASSETS/${url##*/}" "$out" ;;
  *) echo "fake curl: $url" >&2; exit 22 ;;
esac
`

type env struct {
	home, app, assets, args, curlLog string
	vars                             []string
}

func setup(t *testing.T, sums func(tarball []byte) string) env {
	t.Helper()
	root := t.TempDir()
	e := env{home: filepath.Join(root, "home"), assets: filepath.Join(root, "assets"),
		args: filepath.Join(root, "meko-args"), curlLog: filepath.Join(root, "curl-log")}
	e.app = filepath.Join(e.home, ".local", "share", "meko", "app")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{e.home, e.assets, bin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGh), 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	// The release's meko records how it was run.
	meko := "#!/bin/sh\necho \"$*\" > " + e.args + "\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"bin/meko": meko, "VERSION": version + "\n"} {
		h := &tar.Header{Name: "meko-" + version + "/" + name, Mode: 0o755, Size: int64(len(body))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	name := "meko-" + version + "-linux-x64.tar.gz"
	if err := os.WriteFile(filepath.Join(e.assets, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	line := sums(buf.Bytes()) + "  " + name + "\n"
	if err := os.WriteFile(filepath.Join(e.assets, "SHA256SUMS"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	e.vars = []string{"HOME=" + e.home, "PATH=" + bin + ":/usr/bin:/bin", "FAKE_TAG=meko-v" + version,
		"FAKE_ASSETS=" + e.assets, "FAKE_CURL_LOG=" + e.curlLog, "MEKO_TEST_UNAME_S=Linux", "MEKO_TEST_UNAME_M=x86_64", "NO_COLOR=1"}
	return e
}

func goodSums(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (e env) run(t *testing.T, extra []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...) //nolint:gosec // G204: the script under test
	cmd.Env = append(append([]string{}, e.vars...), extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func TestInstallUnpacksTheNewestReleaseAndRunsItsInstall(t *testing.T) {
	e := setup(t, goodSums)
	code, out := e.run(t, nil, "--no-service")
	if code != 0 || !strings.Contains(out, "checksum verified") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(e.app, version, "bin", "meko")); err != nil {
		t.Fatalf("not unpacked: %v", err)
	}
	if b, _ := os.ReadFile(e.args); string(b) != "install --no-service\n" { //nolint:gosec // G304: test file
		t.Errorf("meko ran with %q", b)
	}
	// As meko install would: then the installer says it's there and changes nothing.
	if err := os.Symlink(version, filepath.Join(e.app, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.args); err != nil {
		t.Fatal(err)
	}
	code, out = e.run(t, nil)
	if code != 0 || !strings.Contains(out, "already installed") {
		t.Fatalf("again: exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(e.args); !os.IsNotExist(err) {
		t.Error("meko install ran again")
	}
	code, _ = e.run(t, nil, "--reinstall")
	if b, _ := os.ReadFile(e.args); code != 0 || string(b) != "install\n" { //nolint:gosec // G304: test file
		t.Errorf("--reinstall: exit %d, meko ran with %q", code, b)
	}
}

func TestInstallDownloadsAnonymouslyWithoutGhOrToken(t *testing.T) {
	e := setup(t, goodSums)
	code, out := e.run(t, []string{"FAKE_GH_SIGNED_OUT=1"}, "--no-service")
	if code != 0 || !strings.Contains(out, "checksum verified") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(e.args); string(b) != "install --no-service\n" { //nolint:gosec // G304: test file
		t.Errorf("meko ran with %q", b)
	}
	log, _ := os.ReadFile(e.curlLog) //nolint:gosec // G304: test file
	if strings.Contains(string(log), "api.github.com") || strings.Contains(string(log), "Authorization") {
		t.Errorf("an anonymous install should use neither the API nor a token:\n%s", log)
	}
	if !strings.Contains(string(log), "https://github.com/yugabyte/meko/releases/latest") {
		t.Errorf("the version should come from the latest release's redirect:\n%s", log)
	}
}

func TestInstallSendsGithubTokenWhenGhIsSignedOut(t *testing.T) {
	e := setup(t, goodSums)
	code, out := e.run(t, []string{"FAKE_GH_SIGNED_OUT=1", "GITHUB_TOKEN=tok"}, "--version", version)
	if code != 0 || !strings.Contains(out, "checksum verified") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	log, _ := os.ReadFile(e.curlLog) //nolint:gosec // G304: test file
	if n := strings.Count(string(log), "-H Authorization: Bearer tok"); n != 2 {
		t.Errorf("both downloads should send the token, %d did:\n%s", n, log)
	}
}

func TestInstallNamesSigningInWhenAnAnonymousDownloadFails(t *testing.T) {
	e := setup(t, goodSums)
	for _, args := range [][]string{nil, {"--version", version}} {
		code, out := e.run(t, []string{"FAKE_GH_SIGNED_OUT=1", "FAKE_CURL_404=1"}, args...)
		if code == 0 || !strings.Contains(out, "gh auth login") {
			t.Errorf("%v: exit %d:\n%s", args, code, out)
		}
	}
}

func TestInstallRunsNothingWhenCutShort(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Cut off before the last line, it only defines main.
	cut := strings.TrimRight(string(b), "\n")
	cut = cut[:strings.LastIndex(cut, "\n")]
	e := setup(t, goodSums)
	cmd := exec.Command("sh", "-s", "--", "--no-service")
	cmd.Env = e.vars
	cmd.Stdin = strings.NewReader(cut)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("err %v, output:\n%s", err, out)
	}
	if _, err := os.Stat(e.app); !os.IsNotExist(err) {
		t.Error("a cut-short script installed something")
	}
}

func TestInstallRefusesABadChecksum(t *testing.T) {
	e := setup(t, func([]byte) string { return strings.Repeat("0", 64) })
	code, out := e.run(t, nil)
	if code == 0 || !strings.Contains(out, "checksum verification failed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(e.app, version)); !os.IsNotExist(err) {
		t.Error("a release with a bad checksum was unpacked")
	}
}

func TestInstallTakesOnlyItsOwnFlags(t *testing.T) {
	e := setup(t, goodSums)
	for _, flag := range []string{"--meko-key", "--no-meko", "--repos-root"} {
		code, out := e.run(t, nil, flag, "x")
		if code == 0 || !strings.Contains(out, "unknown option "+flag) ||
			!strings.Contains(out, "meko extension install agentk") {
			t.Errorf("%s: exit %d:\n%s", flag, code, out)
		}
	}
}

func TestInstallRefusesUnsupportedMacs(t *testing.T) {
	e := setup(t, goodSums)
	code, out := e.run(t, []string{"MEKO_TEST_UNAME_S=Darwin", "MEKO_TEST_ARM64=0"})
	if code == 0 || !strings.Contains(out, "only Apple Silicon Macs") {
		t.Errorf("Intel: exit %d:\n%s", code, out)
	}
	code, out = e.run(t, []string{"MEKO_TEST_UNAME_S=Darwin", "MEKO_TEST_ARM64=1", "MEKO_TEST_MACOS=13.6"})
	if code == 0 || !strings.Contains(out, "needs macOS 14 or newer") {
		t.Errorf("macOS 13: exit %d:\n%s", code, out)
	}
}
