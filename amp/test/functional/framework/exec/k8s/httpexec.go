package k8s

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
)

// HTTPExecTransport carries HTTP requests into the cluster over `kubectl exec`:
// curl runs inside a pod and the full wire response (status line, headers,
// body) comes back on stdout. No local socket exists at any point, which is the
// property a kubectl port-forward cannot offer — a forward is a long-lived
// local process that dies silently and takes every pinned client with it,
// while each exec is one self-contained API-server round trip on the same
// authenticated channel every other kubectl command already rides.
type HTTPExecTransport struct {
	// Kube must be bound to the pod (and container, if ambiguous) that carries
	// curl (ForPod).
	Kube *Kubectl
	// BaseURL is the in-cluster address curl dials, e.g.
	// "http://amp-controller:8080" — the Service DNS name works because curl
	// runs inside the namespace.
	BaseURL string
	// EnsurePod, when set, is invoked once per request after an exec-level
	// failure (relay pod evicted, node scaled away) to recreate the pod before
	// the single retry. The pod is stateless, so recreation is always safe.
	EnsurePod func(ctx context.Context) error
	// Timeout bounds one curl attempt. Zero means defaultHTTPExecTimeout.
	Timeout time.Duration

	Logf func(string, ...any)
}

const (
	defaultHTTPExecTimeout = 2 * time.Minute
	// A Service VIP keeps routing to a just-deleted pod, so the connect
	// blackholes; without this it burns the whole --max-time.
	curlConnectTimeout = 10 * time.Second
	transientAttempts  = 3
)

var transientRetryPause = 3 * time.Second

func (t *HTTPExecTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := requestBody(req)
	if err != nil {
		return nil, err
	}
	res, err := t.execTransient(req, body)
	if err == nil {
		return parseWireResponse(res.Stdout, req)
	}
	if t.EnsurePod == nil {
		return nil, err
	}
	if t.Logf != nil {
		t.Logf("k8s: HTTP exec failed (%v); recreating the relay pod and retrying %s %s", err, req.Method, req.URL.Path)
	}
	if eerr := t.EnsurePod(req.Context()); eerr != nil {
		return nil, fmt.Errorf("HTTP exec failed (%w) and the relay pod could not be recreated: %v", err, eerr)
	}
	res, err = t.execTransient(req, body)
	if err != nil {
		return nil, err
	}
	return parseWireResponse(res.Stdout, req)
}

// execTransient retries curl exits where the connection never carried a
// response. Idempotent methods only: a timed-out POST may already have applied.
func (t *HTTPExecTransport) execTransient(req *http.Request, body []byte) (execpkg.Result, error) {
	res, err := t.exec(req, body)
	if err == nil || !idempotentMethod(req.Method) {
		return res, err
	}
	for attempt := 2; attempt <= transientAttempts && transientCurlExit(res.ExitCode); attempt++ {
		if t.Logf != nil {
			t.Logf("k8s: curl exited %d on %s %s; retrying (attempt %d/%d)", res.ExitCode, req.Method, req.URL.Path, attempt, transientAttempts)
		}
		select {
		case <-req.Context().Done():
			return res, err
		case <-time.After(transientRetryPause):
		}
		res, err = t.exec(req, body)
		if err == nil {
			return res, nil
		}
	}
	return res, err
}

func transientCurlExit(code int) bool {
	switch code {
	case 6, 7, 28, 52, 56: // resolve, refused, timed out, empty reply, recv failure
		return true
	}
	return false
}

func idempotentMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// exec runs one curl inside the pod, body on stdin.
func (t *HTTPExecTransport) exec(req *http.Request, body []byte) (execpkg.Result, error) {
	timeout := t.Timeout
	if timeout == 0 {
		timeout = defaultHTTPExecTimeout
	}
	cmd := curlCommand(req, t.BaseURL, len(body) > 0, timeout)
	var stdin io.Reader
	if len(body) > 0 {
		stdin = bytes.NewReader(body)
	}
	res, err := t.Kube.RunStdin(req.Context(), stdin, cmd)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("curl %s %s exited %d: %s", req.Method, req.URL.Path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

// curlCommand renders the curl invocation. -i keeps the status line and
// headers so the caller gets a real *http.Response; "Expect:" suppresses the
// 100-continue interim response that would otherwise prefix the output.
func curlCommand(req *http.Request, baseURL string, hasBody bool, timeout time.Duration) string {
	args := []string{
		"curl", "-isS",
		"--max-time", fmt.Sprintf("%d", int(timeout.Seconds())),
		"--connect-timeout", fmt.Sprintf("%d", int(curlConnectTimeout.Seconds())),
		"-X", req.Method,
		"-H", "Expect:",
	}
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range req.Header.Values(k) {
			args = append(args, "-H", k+": "+v)
		}
	}
	if hasBody {
		args = append(args, "--data-binary", "@-")
	}
	args = append(args, baseURL+req.URL.RequestURI())
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shQuote(a)
	}
	return strings.Join(quoted, " ")
}

// requestBody drains the request body into memory so a failed exec can be
// retried. Controller payloads are small JSON documents.
func requestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return b, nil
}

// parseWireResponse turns curl -i output into a real *http.Response.
func parseWireResponse(wire string, req *http.Request) (*http.Response, error) {
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(stripDecodedEncodings(wire))), req)
	if err != nil {
		return nil, fmt.Errorf("parse HTTP response from exec curl: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read HTTP response body from exec curl: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// stripDecodedEncodings drops Transfer-Encoding from the header block. curl
// de-chunks the body before printing it but still prints the header, so a
// parser trusting that header tries to de-chunk plain bytes and fails ("read
// HTTP response body from exec curl: chunked ..."). Only responses large
// enough for the controller to stream hit it, which is why a list endpoint
// found this and every small call passed. Without the header and without
// Content-Length the body is read to EOF, which is exactly right for a
// response already buffered in full.
func stripDecodedEncodings(wire string) string {
	eol := "\r\n"
	head, body, found := strings.Cut(wire, "\r\n\r\n")
	if !found {
		eol = "\n"
		head, body, found = strings.Cut(wire, "\n\n")
		if !found {
			return wire
		}
	}
	lines := strings.Split(head, eol)
	kept := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(strings.ToLower(ln), "transfer-encoding:") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, eol) + eol + eol + body
}

// shQuote single-quotes s for sh -c, escaping embedded single quotes.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
