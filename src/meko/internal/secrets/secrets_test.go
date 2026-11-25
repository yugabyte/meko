package secrets

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secret = "0123456789abcdef0123456789abcdef"

func TestFilesKeepSecretsPrivate(t *testing.T) {
	root := t.TempDir()
	f := NewFiles(root)
	if _, err := f.Get("client-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := f.Set("client-token", secret); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Get("client-token"); err != nil || got != secret {
		t.Fatalf("get %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "secrets")); fi.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v", fi.Mode().Perm())
	}
	path := filepath.Join(root, "secrets", "client-token")
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("client-token"); err == nil || !strings.Contains(err.Error(), "open to other users") {
		t.Fatalf("an open file must be refused: %v", err)
	}
	if err := f.Delete("client-token"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("client-token"); err != nil {
		t.Fatalf("deleting a missing secret: %v", err)
	}
	if _, err := f.Get("client-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestBadNamesAndValuesAreRefused(t *testing.T) {
	f := NewFiles(t.TempDir())
	for _, n := range []string{"", "../x", "A", "a b"} {
		if err := f.Set(n, secret); err == nil {
			t.Errorf("name %q was taken", n)
		}
	}
	for _, v := range []string{"", "has space", `a"b`, `a\b`, "a\nb"} {
		if err := f.Set("x", v); err == nil {
			t.Errorf("value %q was taken", v)
		}
	}
}

// fakeSecurity stands in for /usr/bin/security: items are files in dir, and every command line it
// gets as arguments is appended to dir/argv.
const fakeSecurity = `#!/bin/sh
dir=$FAKE_KEYCHAIN
echo "$*" >> "$dir/argv"
item() { printf '%s' "$1" | tr '/' '_'; }
cmd() {
  op=$1; shift
  acct=""; val=""; upd=0
  while [ $# -gt 0 ]; do
    case $1 in
      -a) acct=$2; shift 2 ;;
      -w) if [ "$op" = add-generic-password ]; then val=$2; shift 2; else shift; fi ;;
      -s) shift 2 ;;
      -U) upd=1; shift ;;
      *) shift ;;
    esac
  done
  f="$dir/$(item "$acct")"
  case $op in
    find-generic-password)
      [ -f "$f" ] || { echo "security: The specified item could not be found in the keychain." >&2; exit 44; }
      cat "$f"; echo ;;
    delete-generic-password) [ -f "$f" ] || exit 44; rm -f "$f" ;;
    add-generic-password)
      if [ "$upd" = 0 ] && [ -f "$f" ]; then
        echo "security: The specified item already exists in the keychain." >&2; return 45
      fi
      printf '%s' "$val" > "$f" ;;
  esac
}
if [ "$1" = -i ]; then
  while IFS= read -r line; do eval "set -- $line"; ( cmd "$@" ); done
  exit 0
fi
cmd "$@"
`

func useFakeSecurity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "security")
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(bin, []byte(fakeSecurity), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_KEYCHAIN", dir)
	old := securityPath
	securityPath = bin
	t.Cleanup(func() { securityPath = old })
	return dir
}

func TestKeychainThroughSecurity(t *testing.T) {
	dir := useFakeSecurity(t)
	k, err := Open(KindKeychain, "/data/one")
	if err != nil {
		t.Fatal(err)
	}
	other := NewKeychain("/data/two")
	if _, err = k.Get("client-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err = k.Set("client-token", secret); err != nil {
		t.Fatal(err)
	}
	if got, gerr := k.Get("client-token"); gerr != nil || got != secret {
		t.Fatalf("get %q, %v", got, gerr)
	}
	if _, err = other.Get("client-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another data folder sees the secret: %v", err)
	}
	if err = k.Delete("client-token"); err != nil {
		t.Fatal(err)
	}
	if err = k.Delete("client-token"); err != nil {
		t.Fatalf("deleting a missing secret: %v", err)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // G304: the fake's own log
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), secret) {
		t.Fatalf("the secret reached security's arguments:\n%s", argv)
	}
}

func TestKeychainAddKeepsTheFirst(t *testing.T) {
	useFakeSecurity(t)
	k := NewKeychain("/data/one")
	got, err := k.Add("client-token", secret)
	if err != nil || got != secret {
		t.Fatalf("first add: %q, %v", got, err)
	}
	got, err = k.Add("client-token", secret+"ff")
	if err != nil || got != secret {
		t.Fatalf("second add: %q, %v (want the first one kept)", got, err)
	}
}

func TestKeychainErrorsNameTheFix(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "security")
	script := "#!/bin/sh\necho 'security: SecKeychainItemCopyContent: User interaction is not allowed.' >&2\nexit 36\n"
	//nolint:gosec // G306: a test script
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	old := securityPath
	securityPath = bin
	t.Cleanup(func() { securityPath = old })
	_, err := NewKeychain("/data").Get("client-token")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "unlock-keychain") ||
		!strings.Contains(err.Error(), "User interaction is not allowed") {
		t.Fatalf("got %v", err)
	}
}

func TestOpen(t *testing.T) {
	if _, err := Open("vault", t.TempDir()); err == nil {
		t.Error("an unknown kind was taken")
	}
	s, err := Open("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"darwin": KindKeychain}[runtime.GOOS]; want != "" && s.Kind() != want ||
		want == "" && s.Kind() != KindFile {
		t.Errorf("default kind %q on %s", s.Kind(), runtime.GOOS)
	}
	if runtime.GOOS != "darwin" {
		if _, err := Open(KindKeychain, t.TempDir()); err == nil {
			t.Error("the Keychain was opened off macOS")
		}
	}
}

// TestRealKeychain runs on macOS only, against a Keychain file of its own: never the login one.
func TestRealKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Keychain is macOS only")
	}
	kc := filepath.Join(t.TempDir(), "meko-test.keychain-db")
	for _, args := range [][]string{{"create-keychain", "-p", "test", kc}, {"unlock-keychain", "-p", "test", kc}} {
		if out, err := exec.Command(defaultSecurity, args...).CombinedOutput(); err != nil { //nolint:gosec // G204: test
			t.Fatalf("security %v: %v %s", args, err, out)
		}
	}
	t.Cleanup(func() { _ = exec.Command(defaultSecurity, "delete-keychain", kc).Run() }) //nolint:gosec // G204: test
	k := &Keychain{root: t.TempDir(), keychain: kc}
	if _, err := k.Get("client-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := k.Set("client-token", secret); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Add("client-token", "0000"); err != nil || got != secret {
		t.Fatalf("add over an existing item: %q, %v (want it kept)", got, err)
	}
	if err := k.Set("client-token", secret+"ff"); err != nil {
		t.Fatalf("replacing: %v", err)
	}
	if got, err := k.Get("client-token"); err != nil || got != secret+"ff" {
		t.Fatalf("get %q, %v", got, err)
	}
	if err := k.Delete("client-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get("client-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
