// Package client holds the thin HTTP clients the Go yb_amp_local launcher uses
// to drive the controller and the co-located storage daemons (pageserver,
// safekeeper) plus each endpoint's compute_ctl. They are straight ports of the
// reqwest-based clients in control_plane/src/{controller,pageserver,safekeeper,
// endpoint}.rs: same paths, methods, and JSON wire types. LSNs travel as
// "X/Y" hex strings on the wire, so they are modeled as Go strings here.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPError is returned when a daemon answers with a non-2xx status. It carries
// enough context (method, url, status, response body) for the launcher to log a
// useful message, mirroring the reqwest `error_from_body` behavior.
type HTTPError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s %s: http %d: %s", e.Method, e.URL, e.Status, e.Body)
	}
	return fmt.Sprintf("%s %s: http %d", e.Method, e.URL, e.Status)
}

// transport bundles a base URL, an *http.Client, and an optional bearer token
// shared by every request a given client makes.
type transport struct {
	base   string
	hc     *http.Client
	bearer string // empty = no Authorization header
}

// normalizeBase trims a trailing slash and prepends the http scheme when the
// caller passed a bare host:port (the controller's listen addr form).
func normalizeBase(base string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	return base
}

func newTransport(base string, hc *http.Client, bearer string) transport {
	if hc == nil {
		hc = http.DefaultClient
	}
	return transport{base: normalizeBase(base), hc: hc, bearer: bearer}
}

// do issues method+path against the base URL. When body is non-nil it is sent
// as a JSON payload; when out is non-nil the (2xx) response body is decoded into
// it as JSON. A non-2xx status yields an *HTTPError.
func (t *transport) do(ctx context.Context, method, path string, body, out any) error {
	url := t.base + path

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s request: %w", method, url, err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+t.bearer)
	}

	resp, err := t.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return &HTTPError{
			Method: method,
			URL:    url,
			Status: resp.StatusCode,
			Body:   strings.TrimSpace(string(msg)),
		}
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, url, err)
		}
		return nil
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
