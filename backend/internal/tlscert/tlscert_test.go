package tlscert

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelfSigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	now := time.Now()
	p, err := SelfSigned(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !p.SelfSigned || len(p.Fingerprint) != 95 || p.NotAfter.Sub(now) < Lifetime-2*time.Hour {
		t.Fatalf("pair %+v", p)
	}
	for _, name := range []string{"localhost", "127.0.0.1"} {
		if verr := p.Cert.Leaf.VerifyHostname(name); verr != nil {
			t.Errorf("%s: %v", name, verr)
		}
	}
	if fi, serr := os.Stat(filepath.Join(dir, KeyName)); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi.Mode(), serr)
	}

	// The same pair at the next start, and a new one when it's about to expire.
	again, err := SelfSigned(dir, now.Add(time.Hour))
	if err != nil || again.Fingerprint != p.Fingerprint {
		t.Fatalf("kept: %v, %s vs %s", err, again.Fingerprint, p.Fingerprint)
	}
	renewed, err := SelfSigned(dir, now.Add(Lifetime-RenewBefore+time.Hour))
	if err != nil || renewed.Fingerprint == p.Fingerprint {
		t.Fatalf("renewed: %v, same fingerprint %v", err, renewed.Fingerprint == p.Fingerprint)
	}

	// The [tls] files load as they are, and say they aren't Meko's own.
	loaded, err := Load(filepath.Join(dir, CertName), filepath.Join(dir, KeyName))
	if err != nil || loaded.SelfSigned || loaded.Fingerprint != renewed.Fingerprint {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.crt"), filepath.Join(dir, KeyName)); err == nil {
		t.Fatal("a missing cert loaded")
	}
}
