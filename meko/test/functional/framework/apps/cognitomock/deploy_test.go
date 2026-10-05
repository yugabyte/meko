package cognitomock

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mockpod"
)

func TestMaterialServesTheCognitoHostnameUnderItsOwnCA(t *testing.T) {
	m, err := NewMaterial(Host("us-east-1"), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(m.CAPEM) {
		t.Fatal("ca does not parse")
	}
	block, _ := pem.Decode(m.CertPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "cognito-idp.us-east-1.amazonaws.com", Roots: roots}); err != nil {
		t.Fatalf("leaf does not verify for the cognito host: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: roots}); err == nil {
		t.Fatal("leaf verifies for a host it was not issued for")
	}
}

func TestManifestHasTheThreeObjectsAndPutsTheKeysInTheSecret(t *testing.T) {
	m, err := NewMaterial(Host(DefaultRegion), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Spec{ClientSecret: "cs"}.Manifest(m, mockpod.Binary{Path: "/x/mekoqa", SHA: "0123456789abcdef0123"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		kinds[doc["kind"].(string)] = doc
	}
	for _, k := range []string{"Secret", "Deployment", "Service"} {
		if kinds[k] == nil {
			t.Errorf("no %s in the manifest", k)
		}
	}
	if kinds["ConfigMap"] != nil {
		t.Error("the manifest still carries a ConfigMap")
	}
	secret, _ := yaml.Marshal(kinds["Secret"])
	for _, want := range []string{"client-secret: cs", "ca.crt", "tls.crt", "tls.key", "token.key"} {
		if !strings.Contains(string(secret), want) {
			t.Errorf("the Secret lacks %s", want)
		}
	}
	dep, _ := yaml.Marshal(kinds["Deployment"])
	if strings.Contains(string(dep), "value: cs") {
		t.Error("the client secret is in the Deployment spec")
	}
	for _, want := range []string{"mirror.gcr.io/library/alpine:3.20", "/opt/mekoqa/mekoqa", "- cognito", "--pool-id", "mekoqa/bin: 0123456789abcdef", "secretKeyRef"} {
		if !strings.Contains(string(dep), want) {
			t.Errorf("the Deployment lacks %s", want)
		}
	}
}

func TestIssuerMatchesWhatTheApiServerBuilds(t *testing.T) {
	got := Spec{}.Issuer()
	if got != "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_MekoQa001" {
		t.Fatalf("issuer %q", got)
	}
}
