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
  "auth status") exit 0 ;;
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

type env struct {
	home, app, assets, args string
	vars                    []string
}

func setup(t *testing.T, sums func(tarball []byte) string) env {
	t.Helper()
	root := t.TempDir()
	e := env{home: filepath.Join(root, "home"), assets: filepath.Join(root, "assets"),
		args: filepath.Join(root, "meko-args")}
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
		"FAKE_ASSETS=" + e.assets, "MEKO_TEST_UNAME_S=Linux", "MEKO_TEST_UNAME_M=x86_64", "NO_COLOR=1"}
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
