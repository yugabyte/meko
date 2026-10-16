package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

// curl exit 28 is what a nemesis-killed target pod leaves behind.
func TestExecTransportRetriesTransientCurlExitsOnGET(t *testing.T) {
	old := transientRetryPause
	transientRetryPause = time.Millisecond
	defer func() { transientRetryPause = old }()

	attempts := 0
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			attempts++
			if attempts == 1 {
				return execpkg.Result{ExitCode: 28, Stderr: "curl: (28) Connection timed out"}, nil
			}
			return execpkg.Result{Stdout: "HTTP/1.1 200 OK\r\n\r\n{\"state\":\"succeeded\"}"}, nil
		})
	tr := &HTTPExecTransport{Kube: kube, BaseURL: "http://amp-controller:8080", Logf: func(string, ...any) {}}
	req, _ := http.NewRequest(http.MethodGet, "http://placeholder/api/v1/tasks/t1", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("want the retry to succeed, got %v", err)
	}
	resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// A timed-out POST may already have applied; retrying could double-create.
func TestExecTransportDoesNotRetryTransientExitsOnPOST(t *testing.T) {
	attempts := 0
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			attempts++
			return execpkg.Result{ExitCode: 28, Stderr: "curl: (28) Connection timed out"}, nil
		})
	tr := &HTTPExecTransport{Kube: kube, BaseURL: "http://amp-controller:8080"}
	req, _ := http.NewRequest(http.MethodPost, "http://placeholder/api/v1/clusters", strings.NewReader("{}"))
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("want the timeout surfaced")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestExecTransportGivesUpAfterTransientBudget(t *testing.T) {
	old := transientRetryPause
	transientRetryPause = time.Millisecond
	defer func() { transientRetryPause = old }()

	attempts := 0
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			attempts++
			return execpkg.Result{ExitCode: 7, Stderr: "curl: (7) Connection refused"}, nil
		})
	tr := &HTTPExecTransport{Kube: kube, BaseURL: "http://amp-controller:8080"}
	req, _ := http.NewRequest(http.MethodGet, "http://placeholder/healthz", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("want the failure surfaced once the budget is spent")
	}
	if attempts != transientAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, transientAttempts)
	}
}

const apiServerTimeout = "Unable to connect to the server: dial tcp 10.150.8.6:443: i/o timeout"

func TestExecTransportRetriesAPIServerUnreachableOnPOST(t *testing.T) {
	old := transientRetryPause
	transientRetryPause = time.Millisecond
	defer func() { transientRetryPause = old }()

	attempts := 0
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			attempts++
			if attempts == 1 {
				return execpkg.Result{ExitCode: 1, Stderr: apiServerTimeout}, nil
			}
			return execpkg.Result{Stdout: "HTTP/1.1 202 Accepted\r\n\r\n{}"}, nil
		})
	tr := &HTTPExecTransport{Kube: kube, BaseURL: "http://amp-controller:8080", Logf: func(string, ...any) {}}
	req, _ := http.NewRequest(http.MethodPost, "http://placeholder/api/v1/endpoint-upgrades", strings.NewReader("{}"))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("want the retry to succeed, got %v", err)
	}
	resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestExecTransportTypesAPIServerUnreachableOnceTheBudgetIsSpent(t *testing.T) {
	old := transientRetryPause
	transientRetryPause = time.Millisecond
	defer func() { transientRetryPause = old }()

	attempts := 0
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			attempts++
			return execpkg.Result{ExitCode: 1, Stderr: apiServerTimeout}, nil
		})
	ensured := false
	tr := &HTTPExecTransport{
		Kube: kube, BaseURL: "http://amp-controller:8080",
		EnsurePod: func(context.Context) error { ensured = true; return nil },
	}
	req, _ := http.NewRequest(http.MethodPost, "http://placeholder/api/v1/endpoint-upgrades", strings.NewReader("{}"))
	_, err := tr.RoundTrip(req)
	var unreachable *APIServerUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("err = %v, want an APIServerUnreachableError so the result lands env_failed", err)
	}
	if attempts != transientAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, transientAttempts)
	}
	if ensured {
		t.Fatal("recreated the relay pod although the API server, not the pod, was unreachable")
	}
}

// kubectl says "Unable to connect" for a mid-stream EOF too, after the exec
// upgrade may already have started curl; replaying the POST could double-create.
func TestExecTransportDoesNotReplayPOSTOnAnAPIServerEOF(t *testing.T) {
	attempts := 0
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
			attempts++
			return execpkg.Result{ExitCode: 1, Stderr: "Unable to connect to the server: unexpected EOF"}, nil
		})
	tr := &HTTPExecTransport{Kube: kube, BaseURL: "http://amp-controller:8080"}
	req, _ := http.NewRequest(http.MethodPost, "http://placeholder/api/v1/clusters", strings.NewReader("{}"))
	_, err := tr.RoundTrip(req)
	var unreachable *APIServerUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("err = %v, want an APIServerUnreachableError", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestExecTransportKeepsTheRelayPodOnAnUnreachableKubelet(t *testing.T) {
	defer func(d time.Duration) { kubeletRetryDelay = d }(kubeletRetryDelay)
	kubeletRetryDelay = time.Millisecond
	kube := New(Config{Namespace: "ns", Pod: "relay", Container: "relay"}).WithRunner(
		func(context.Context, io.Reader, []string) (execpkg.Result, error) {
			return execpkg.Result{ExitCode: 1, Stderr: execDialFail}, nil
		})
	ensured := false
	tr := &HTTPExecTransport{
		Kube: kube, BaseURL: "http://amp-controller:8080",
		EnsurePod: func(context.Context) error { ensured = true; return nil },
	}
	req, _ := http.NewRequest(http.MethodGet, "http://placeholder/api/v1/tasks/t1", nil)
	_, err := tr.RoundTrip(req)
	var unreachable *KubeletUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("err = %v, want a KubeletUnreachableError", err)
	}
	if ensured {
		t.Fatal("recreated the relay pod, which lands it on the same node")
	}
}
