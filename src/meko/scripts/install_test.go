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

// fakeGh is a gh that's signed in and records being run: the installer never runs it.
const fakeGh = `#!/bin/sh
echo "$*" >> "$FAKE_GH_LOG"
`

// fakeCurl answers install.sh's curl calls: the latest release's redirect and the asset
// downloads, logging each call's arguments. FAKE_CURL_404 answers 404.
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
	home, app, assets, args, curlLog, ghLog string
	vars                                    []string
}

func setup(t *testing.T, sums func(tarball []byte) string) env {
	t.Helper()
	root := t.TempDir()
	e := env{home: filepath.Join(root, "home"), assets: filepath.Join(root, "assets"),
		args: filepath.Join(root, "meko-args"), curlLog: filepath.Join(root, "curl-log"),
		ghLog: filepath.Join(root, "gh-log")}
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
	// The release's meko records how it was run, and what the installer told it.
	meko := "#!/bin/sh\necho \"$*\" > " + e.args + "\nenv | grep '^MEKO_INSTALL_' | sort > " + e.args + ".env\n"
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
		"FAKE_ASSETS=" + e.assets, "FAKE_CURL_LOG=" + e.curlLog, "FAKE_GH_LOG=" + e.ghLog,
		"MEKO_TEST_UNAME_S=Linux", "MEKO_TEST_UNAME_M=x86_64", "NO_COLOR=1"}
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
	if code != 0 || !strings.Contains(out, "  ✓ Download        meko "+version+" · 0 MB in ") ||
		!strings.Contains(out, " · checksum verified\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "  ◆ meko  "+version+" · Linux · x64\n    Installs into your home folder, no sudo\n") {
		t.Errorf("no header:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.app, version, "bin", "meko")); err != nil {
		t.Fatalf("not unpacked: %v", err)
	}
	if b, _ := os.ReadFile(e.args); string(b) != "install --no-service\n" { //nolint:gosec // G304: test file
		t.Errorf("meko ran with %q", b)
	}
	// meko install is told it's the first install, that it may ask, and when the run began.
	told, _ := os.ReadFile(e.args + ".env") //nolint:gosec // G304: test file
	for _, want := range []string{"MEKO_INSTALL_ASK=1\n", "MEKO_INSTALL_FIRST=1\n", "MEKO_INSTALL_FROM=\n",
		"MEKO_INSTALL_STARTED=1"} {
		if !strings.Contains(string(told), want) {
			t.Errorf("meko install wasn't told %q:\n%s", want, told)
		}
	}
	// As meko install would: then the installer says it's up to date, and changes nothing.
	if err := os.Symlink(version, filepath.Join(e.app, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.args); err != nil {
		t.Fatal(err)
	}
	code, out = e.run(t, nil)
	if code != 0 || out != "  ✓ Meko "+version+" is up to date · --reinstall sets it up again\n" {
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

// Run again with a newer release out, the installer upgrades: its header names both versions, and
// meko install is told what it replaces, and that it isn't the first install.
func TestInstallUpgradesAnOlderRelease(t *testing.T) {
	e := setup(t, goodSums)
	old := filepath.Join(e.app, "0.20261001.1", "bin")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(filepath.Join(old, "meko"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("0.20261001.1", filepath.Join(e.app, "current")); err != nil {
		t.Fatal(err)
	}
	code, out := e.run(t, nil, "--non-interactive")
	if code != 0 || !strings.Contains(out, "  ◆ meko  0.20261001.1 → "+version+" · Linux · x64\n\n") ||
		strings.Contains(out, "no sudo") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(e.args); string(b) != "install\n" { //nolint:gosec // G304: test file
		t.Errorf("meko ran with %q", b)
	}
	told, _ := os.ReadFile(e.args + ".env") //nolint:gosec // G304: test file
	for _, want := range []string{"MEKO_INSTALL_ASK=0\n", "MEKO_INSTALL_FIRST=\n", "MEKO_INSTALL_FROM=0.20261001.1\n"} {
		if !strings.Contains(string(told), want) {
			t.Errorf("meko install wasn't told %q:\n%s", want, told)
		}
	}
}

// The installer downloads with curl alone, with no sign-in: never gh, even signed in, never the API
// and never a token, even with GITHUB_TOKEN set.
func TestInstallDownloadsAnonymouslyWithCurl(t *testing.T) {
	e := setup(t, goodSums)
	for _, args := range [][]string{{"--no-service"}, {"--version", version, "--reinstall"}} {
		code, out := e.run(t, []string{"GITHUB_TOKEN=test-token"}, args...)
		if code != 0 || !strings.Contains(out, "checksum verified") {
			t.Fatalf("%v: exit %d:\n%s", args, code, out)
		}
	}
	if b, err := os.ReadFile(e.ghLog); err == nil { //nolint:gosec // G304: test file
		t.Errorf("gh ran:\n%s", b)
	}
	log, _ := os.ReadFile(e.curlLog) //nolint:gosec // G304: test file
	if strings.Contains(string(log), "api.github.com") || strings.Contains(string(log), "Authorization") ||
		strings.Contains(string(log), "test-token") {
		t.Errorf("the downloads should use neither the API nor a token:\n%s", log)
	}
	if !strings.Contains(string(log), "https://github.com/yugabyte/meko/releases/latest") {
		t.Errorf("the version should come from the latest release's redirect:\n%s", log)
	}
}

// A failed download says what failed, and that running again is safe, with no sign-in hint.
func TestInstallSaysWhenADownloadFails(t *testing.T) {
	e := setup(t, goodSums)
	code, out := e.run(t, []string{"FAKE_CURL_404=1"})
	if code == 0 || !strings.Contains(out, "could not find a Meko release in yugabyte/meko") {
		t.Errorf("no release: exit %d:\n%s", code, out)
	}
	code, out = e.run(t, []string{"FAKE_CURL_404=1"}, "--version", version)
	if code == 0 || !strings.Contains(out, "  ✗ the download of meko-"+version+"-linux-x64.tar.gz failed\n") ||
		!strings.Contains(out, "running the installer again is safe\n") || strings.Contains(out, "gh auth") {
		t.Errorf("--version: exit %d:\n%s", code, out)
	}
}

// Off a terminal, the run has no colors, no dots and no carriage returns, only plain lines.
func TestInstallIsPlainOffATerminal(t *testing.T) {
	e := setup(t, goodSums)
	code, out := e.run(t, []string{"NO_COLOR="}, "--no-service")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.ContainsAny(out, "\r\033") || strings.Contains(out, "⣾") {
		t.Errorf("escape codes or dots in:\n%q", out)
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

func TestInstallHelpPrintsTheHeaderOnly(t *testing.T) {
	e := setup(t, goodSums)
	for _, flag := range []string{"--help", "-h"} {
		code, out := e.run(t, nil, flag)
		if code != 0 {
			t.Fatalf("%s: exit %d:\n%s", flag, code, out)
		}
		for _, want := range []string{"curl -fsSL https://mekodata.ai/install.sh | sh", "--version <X.Y.Z>",
			"--reinstall", "--no-service", "--non-interactive", "NO_COLOR"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q in:\n%s", flag, want, out)
			}
		}
		if strings.Contains(out, "#") || strings.Contains(out, "set -eu") || strings.Contains(out, "GITHUB_TOKEN") {
			t.Errorf("%s: printed past the header or kept its #:\n%s", flag, out)
		}
		if _, err := os.Stat(e.app); !os.IsNotExist(err) {
			t.Errorf("%s: installed something", flag)
		}
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
