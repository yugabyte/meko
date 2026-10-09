package deploy

import (
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An empty list must remove the field rather than set it to []: the chart omits
// it for an empty list, and a Service carrying [] would differ from the next
// render of the release.
func TestSourceRangesPatch(t *testing.T) {
	for name, tc := range map[string]struct {
		cidrs []string
		want  string
	}{
		"replaces the list": {[]string{"10.0.0.0/16", "192.0.2.10/32"},
			`{"spec":{"loadBalancerSourceRanges":["10.0.0.0/16","192.0.2.10/32"]}}`},
		"empty removes it": {[]string{}, `{"spec":{"loadBalancerSourceRanges":null}}`},
		"nil removes it":   {nil, `{"spec":{"loadBalancerSourceRanges":null}}`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := sourceRangesPatch(tc.cidrs)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("patch = %s, want %s", got, tc.want)
			}
		})
	}
}

// fakeAPIServer stands in for the in-cluster API server: a TLS server whose CA
// and a token are written where the deployer reads the service account.
func fakeAPIServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := serviceAccountDir
	serviceAccountDir = dir
	t.Cleanup(func() { serviceAccountDir = old })
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
}

// The request the API server sees: a merge patch of the chart's -lb Service in
// the deployer's namespace, authenticated as the pod, carrying only the ranges.
func TestUpdateSourceRangesPatchesTheLBService(t *testing.T) {
	var method, path, ctype, auth, body string
	fakeAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, ctype, auth, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)
		_, _ = io.WriteString(w, `{}`)
	})
	k := &KubernetesDeployer{cfg: KubernetesConfig{Namespace: "endpoints"}, logger: slog.New(slog.DiscardHandler)}

	if err := k.UpdateSourceRanges(t.Context(), "yb-ep-1", []string{"192.0.2.10/32"}); err != nil {
		t.Fatalf("UpdateSourceRanges: %v", err)
	}
	if method != http.MethodPatch || path != "/api/v1/namespaces/endpoints/services/yb-ep-1-lb" {
		t.Errorf("request = %s %s, want PATCH of the endpoint's -lb Service", method, path)
	}
	if ctype != "application/merge-patch+json" {
		t.Errorf("content type = %q, want a JSON merge patch, which replaces the list whole", ctype)
	}
	if auth != "Bearer sa-token" {
		t.Errorf("authorization = %q, want the service-account token", auth)
	}
	if body != `{"spec":{"loadBalancerSourceRanges":["192.0.2.10/32"]}}` {
		t.Errorf("body = %s, want only the source ranges", body)
	}
}

// A Service that is not there — the endpoint is not nlb_*, or its release is gone
// — must fail the task, so the row is not written for a change that never landed.
func TestUpdateSourceRangesFailsWhenTheServiceIsMissing(t *testing.T) {
	fakeAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound"}`)
	})
	k := &KubernetesDeployer{cfg: KubernetesConfig{Namespace: "endpoints"}, logger: slog.New(slog.DiscardHandler)}

	err := k.UpdateSourceRanges(t.Context(), "yb-ep-1", []string{"192.0.2.10/32"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404 reported", err)
	}
}
