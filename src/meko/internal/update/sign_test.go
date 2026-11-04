package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// testKey is a generated Ed25519 key pair.
func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// sigLine is priv's line of a SHA256SUMS.sig for sums, as the release workflows write it.
func sigLine(priv ed25519.PrivateKey, sums string) string {
	pub, _ := priv.Public().(ed25519.PublicKey)
	return KeyID(pub) + " " +
		base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(sums))) + "\n"
}

// verify runs VerifySumsSignature on sums and sig written to files.
func verify(t *testing.T, sums, sig string, keys ...ed25519.PublicKey) error {
	t.Helper()
	dir := t.TempDir()
	sumsFile, sigFile := filepath.Join(dir, Sums), filepath.Join(dir, SumsSig)
	if err := os.WriteFile(sumsFile, []byte(sums), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sigFile, []byte(sig), 0o600); err != nil {
		t.Fatal(err)
	}
	return VerifySumsSignature(sumsFile, sigFile, keys)
}

func TestVerifySumsSignature(t *testing.T) {
	const sums = "0123  meko-0.20261103.0-linux-x64.tar.gz\n"
	pubA, privA := testKey(t)
	pubB, privB := testKey(t)
	_, privC := testKey(t)
	good := sigLine(privA, sums)
	flipped := []byte(good) // a base64 digit of the signature changed
	if flipped[20] = 'A'; good[20] == 'A' {
		flipped[20] = 'B'
	}
	short := KeyID(pubA) + " " + base64.StdEncoding.EncodeToString(make([]byte, 63)) + "\n"
	for _, c := range []struct {
		name, sums, sig string
		keys            []ed25519.PublicKey
		want            error
	}{
		{"good", sums, good, []ed25519.PublicKey{pubA}, nil},
		{"by the second of two keys", sums, sigLine(privB, sums), []ed25519.PublicKey{pubA, pubB}, nil},
		{"no final newline", sums, strings.TrimSuffix(good, "\n"), []ed25519.PublicKey{pubA}, nil},
		{"overlap, old Meko", sums, sigLine(privB, sums) + good, []ed25519.PublicKey{pubA}, nil},
		{"overlap, new Meko", sums, sigLine(privB, sums) + good, []ed25519.PublicKey{pubB}, nil},
		{"both lines known", sums, sigLine(privB, sums) + good, []ed25519.PublicKey{pubA, pubB}, nil},
		{"unknown key", sums, sigLine(privC, sums), []ed25519.PublicKey{pubA, pubB}, errUnknownKey},
		{"a changed byte in SHA256SUMS", strings.Replace(sums, "0123", "0124", 1), good,
			[]ed25519.PublicKey{pubA}, errBadSignature},
		{"a changed signature", sums, string(flipped), []ed25519.PublicKey{pubA}, errBadSignature},
		{"another key's signature under A's id", sums,
			KeyID(pubA) + sigLine(privB, sums)[16:], []ed25519.PublicKey{pubA}, errBadSignature},
		{"one known line fails", sums, sigLine(privB, "other") + good, []ed25519.PublicKey{pubA, pubB},
			errBadSignature},
		{"bad base64", sums, KeyID(pubA) + " not*base64\n", []ed25519.PublicKey{pubA}, errBadSignature},
		{"63 bytes", sums, short, []ed25519.PublicKey{pubA}, errBadSignature},
		{"no key id", sums, good[17:], []ed25519.PublicKey{pubA}, errBadSignature},
		{"an id that isn't hex", sums, "ZZZZZZZZZZZZZZZZ" + good[16:], []ed25519.PublicKey{pubA}, errBadSignature},
		{"an uppercase id", sums, strings.ToUpper(good[:16]) + good[16:], []ed25519.PublicKey{pubA}, errBadSignature},
		{"a malformed line by an unknown key", sums, good + "0011223344556677 x\n", []ed25519.PublicKey{pubA},
			errBadSignature},
		{"empty", sums, "", []ed25519.PublicKey{pubA}, errBadSignature},
		{"too large", sums, good + strings.Repeat("\n", maxSig), []ed25519.PublicKey{pubA}, errBadSignature},
	} {
		if err := verify(t, c.sums, c.sig, c.keys...); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

// Meko's own keys parse, and include release-2026.pub's, the key the release environment signs with.
func TestReleaseKeys(t *testing.T) {
	keys, err := parseKeys(keyFiles)
	if err != nil || len(keys) == 0 {
		t.Fatalf("keys %v, %v", keys, err)
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, KeyID(k))
	}
	if !strings.Contains(strings.Join(ids, " "), "8addf981385660f7") {
		t.Errorf("key ids %v lack release-2026.pub's", ids)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"not PEM": "hello",
		"RSA":     string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		"garbage": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})),
	} {
		if _, err := parseKeys(fstest.MapFS{"keys/k.pub": {Data: []byte(data)}}); err == nil {
			t.Errorf("%s parsed", name)
		}
	}
	if _, err := parseKeys(fstest.MapFS{}); err == nil {
		t.Error("no keys parsed")
	}
}

// A SHA256SUMS.sig line made as the release workflows make it, with openssl, verifies here.
func TestOpenSSLSignatureVerifies(t *testing.T) {
	dir := t.TempDir()
	key, sums, sig := filepath.Join(dir, "key.pem"), filepath.Join(dir, Sums), filepath.Join(dir, "sig.bin")
	if err := os.WriteFile(sums, []byte("0123  x.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	openssl := func(stdin []byte, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("openssl", args...)
		cmd.Stdin = bytes.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("openssl can't sign Ed25519 here (OpenSSL 3 has pkeyutl -rawin): %v", err)
		}
		return out
	}
	openssl(nil, "genpkey", "-algorithm", "ed25519", "-out", key)
	openssl(nil, "pkeyutl", "-sign", "-inkey", key, "-rawin", "-in", sums, "-out", sig)
	der := openssl(nil, "pkey", "-in", key, "-pubout", "-outform", "DER")
	id := string(openssl(der[len(der)-32:], "dgst", "-sha256", "-r"))[:16]
	b64 := strings.TrimSpace(string(openssl(nil, "base64", "-A", "-in", sig)))
	if err := os.WriteFile(filepath.Join(dir, SumsSig), []byte(id+" "+b64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	pub, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok {
		t.Fatal(parsed, err)
	}
	if KeyID(pub) != id {
		t.Errorf("KeyID %s, openssl's %s", KeyID(pub), id)
	}
	if err := VerifySumsSignature(sums, filepath.Join(dir, SumsSig), []ed25519.PublicKey{pub}); err != nil {
		t.Fatal(err)
	}
}

// Stage checks the signature before the checksum: a bad one is always refused, and a missing one,
// or one by a key Meko doesn't know, is refused when required and passes with one notice when not.
// A refusal leaves nothing behind.
func TestStageChecksTheSignature(t *testing.T) {
	const next = "0.20261103.0"
	goos, arch := Platform()
	tgz := tarball(t, next)
	sums := sum(tgz) + "  " + TarballName(next, goos, arch) + "\n"
	pub, priv := testKey(t)
	_, other := testKey(t)
	sigs := map[string]string{
		"good":        sigLine(priv, sums),
		"bad":         sigLine(priv, "other"),
		"unknown key": sigLine(other, sums),
		"missing":     "",
	}
	for name, sig := range sigs {
		for _, require := range []bool{true, false} {
			a := newApp(t, "0.20261102.0")
			c := signedReleaseServer(t, next, tgz, sums, sig)
			c.Keys, c.AllowUnsigned = []ed25519.PublicKey{pub}, !require
			var notices []string
			c.Notice = func(s string) { notices = append(notices, s) }
			rels, err := c.Releases(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = a.Stage(context.Background(), c, rels[0], next)
			ok := name == "good" || !require && name != "bad"
			if ok != (err == nil) {
				t.Errorf("%s, require %v: %v", name, require, err)
			}
			wantNotices := 0
			if ok && name != "good" {
				wantNotices = 1
			}
			if len(notices) != wantNotices {
				t.Errorf("%s, require %v: notices %q", name, require, notices)
			}
			tag := TagPrefix + next
			var want string
			switch {
			case ok && name == "missing":
				want = tag + " isn't signed (it has no SHA256SUMS.sig): installing it without that check"
			case ok && name == "unknown key":
				want = tag + " is signed with a key this Meko doesn't know: installing it without that check"
			case name == "bad":
				want = tag + "'s SHA256SUMS.sig doesn't match Meko's release keys, so it won't be installed"
			case name == "missing":
				want = tag + " isn't signed (it has no SHA256SUMS.sig): [update] require_signature = false installs it anyway"
			case name == "unknown key":
				want = tag + " is signed with a key this Meko doesn't know: install Meko again with its one-line install, " +
					"or [update] require_signature = false installs it anyway"
			}
			got := strings.Join(notices, "\n")
			if err != nil {
				got = err.Error()
			}
			if !strings.HasPrefix(got, want) {
				t.Errorf("%s, require %v: %q, want %q", name, require, got, want)
			}
			_, staged := os.Stat(a.Dir(next))
			if ok != (staged == nil) {
				t.Errorf("%s, require %v: staged %v", name, require, staged)
			}
			left, _ := filepath.Glob(filepath.Join(a.Root, ".*-*"))
			if len(left) > 0 {
				t.Errorf("%s, require %v: left %v", name, require, left)
			}
		}
	}

	// A zero Client refuses an unsigned release, and one allowed without the check that then fails
	// its checksum gets no notice: the notice is for a release staged.
	c := signedReleaseServer(t, next, tgz, sums, "")
	rels, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = newApp(t).Stage(context.Background(), c, rels[0], next)
	if err == nil || !strings.Contains(err.Error(), "isn't signed") {
		t.Errorf("a zero Client: %v", err)
	}
	c = signedReleaseServer(t, next, tgz, strings.Repeat("0", 64)+"  "+TarballName(next, goos, arch)+"\n", "")
	c.AllowUnsigned = true
	var notices []string
	c.Notice = func(s string) { notices = append(notices, s) }
	if rels, err = c.Releases(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = newApp(t).Stage(context.Background(), c, rels[0], next)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || len(notices) != 0 {
		t.Errorf("a refused unsigned release: %v, notices %q", err, notices)
	}
}
