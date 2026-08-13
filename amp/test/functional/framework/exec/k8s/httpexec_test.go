package k8s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
)

func execTransportWith(t *testing.T, wire string, fail *int, sawStdin *string) *HTTPExecTransport {
	t.Helper()
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			if stdin != nil && sawStdin != nil {
				b, _ := io.ReadAll(stdin)
				*sawStdin = string(b)
			}
			if fail != nil && *fail > 0 {
				*fail--
				return execpkg.Result{ExitCode: 1, Stderr: "pod not found"}, fmt.Errorf("kubectl exec: pod not found")
			}
			return execpkg.Result{Stdout: wire}, nil
		})
	return &HTTPExecTransport{Kube: kube, BaseURL: "http://amp-controller:8080", Logf: func(string, ...any) {}}
}

func TestExecTransportKeepsStatusHeadersAndBody(t *testing.T) {
	wire := "HTTP/1.1 202 Accepted\r\nContent-Type: application/json\r\nLocation: /api/v1/tasks/abc\r\n\r\n{\"id\":\"t1\"}"
	var body string
	tr := execTransportWith(t, wire, nil, &body)
	req, _ := http.NewRequest(http.MethodPost, "http://placeholder/api/v1/clusters", strings.NewReader(`{"name":"x"}`))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 202 {
		t.Errorf("status = %d, want 202 — tests assert exact codes (409 vs 500)", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/api/v1/tasks/abc" {
		t.Errorf("Location = %q — the async task flow reads it", got)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"t1"}` {
		t.Errorf("body = %q", b)
	}
	if body != `{"name":"x"}` {
		t.Errorf("request body reached curl's stdin as %q", body)
	}
}

func TestExecTransportRecreatesTheRelayOnce(t *testing.T) {
	fails := 1
	tr := execTransportWith(t, "HTTP/1.1 200 OK\r\n\r\nok", &fails, nil)
	ensured := false
	tr.EnsurePod = func(ctx context.Context) error { ensured = true; return nil }
	req, _ := http.NewRequest(http.MethodGet, "http://placeholder/healthz", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("want the retry to succeed after recreating the relay, got %v", err)
	}
	resp.Body.Close()
	if !ensured {
		t.Fatal("the relay pod was not recreated before the retry")
	}
}

func TestExecTransportFailsWithoutEnsurePod(t *testing.T) {
	fails := 2
	tr := execTransportWith(t, "", &fails, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://placeholder/healthz", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("want the exec failure surfaced")
	}
}

func TestCurlCommandShape(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://placeholder/api/v1/pools?limit=5", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/json")
	cmd := curlCommand(req, "http://amp-controller:8080", true, defaultHTTPExecTimeout)
	for _, want := range []string{
		"'-X' 'POST'",
		"'Expect:'", // suppresses the 100-continue interim response -i would capture
		"'Content-Type: application/json'",
		"'--data-binary' '@-'",
		"'http://amp-controller:8080/api/v1/pools?limit=5'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("curl command missing %s:\n%s", want, cmd)
		}
	}
}

// curl de-chunks the body but still prints Transfer-Encoding: chunked, so a
// parser that trusts the header fails on plain bytes. Only responses big
// enough for the controller to stream reach this path — a cluster list did,
// after every smaller call in the same run had passed.
func TestExecTransportHandlesAChunkedHeaderOverDecodedBody(t *testing.T) {
	wire := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n{\"clusters\":[{\"id\":\"c1\"}]}"
	tr := execTransportWith(t, wire, nil, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://placeholder/api/v1/clusters?limit=500", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("a de-chunked body under a chunked header must parse: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"clusters":[{"id":"c1"}]}` {
		t.Errorf("body = %q", b)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
