package stacktest

import (
	"strings"
	"testing"
)

func TestTamperAndForgeKeepTheShapeOfAToken(t *testing.T) {
	const tok = "eyJraWQiOiJrMSIsImFsZyI6IlJTMjU2In0.eyJzdWIiOiJhIiwiZXhwIjoxfQ.c2ln"
	tampered, err := TamperClaims(tok, func(c map[string]any) { c["sub"] = "b" })
	if err != nil {
		t.Fatal(err)
	}
	h, body, sig, err := split(tampered)
	if err != nil || h != "eyJraWQiOiJrMSIsImFsZyI6IlJTMjU2In0" || sig != "c2ln" {
		t.Fatalf("tampered: %v %q %q", err, h, sig)
	}
	if c, _ := claimsOf(body); c["sub"] != "b" || c["exp"] != float64(1) {
		t.Fatalf("claims %v", c)
	}
	forged, err := ForgeJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	fh, fb, fs, _ := split(forged)
	oh, ob, os, _ := split(tok)
	if fh != oh || fb != ob || fs == os || len(fs) < 300 {
		t.Fatalf("forged: head and body must be kept and the signature replaced: %s", forged)
	}
	if _, err := TamperClaims("a.b", nil); err == nil || !strings.Contains(err.Error(), "parts") {
		t.Fatalf("err = %v", err)
	}
}
