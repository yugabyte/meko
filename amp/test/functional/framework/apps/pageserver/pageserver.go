// Package pageserver drives the pageserver's management HTTP API from a
// test: scraping its Prometheus metrics and forcing a flush of a branch's
// buffered pages down to YugabyteDB. Both routes are on the pageserver's
// allowlist, so no token is involved.
//
// The port is container-local in every shape amp ships, so the client talks to
// it through an injected in-container command runner rather than dialling it
// directly. The endpoint image is bare Debian: no curl, no wget, no python3 —
// bash is the one tool guaranteed present, so requests go over its /dev/tcp
// (the same idiom the metrics sampler's scrape uses). The response status line
// is checked in-container: /dev/tcp happily returns a 404 body as stdout.
package pageserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DefaultHTTPPort is the pageserver's management HTTP port (PAGESERVER_HTTP_PORT).
const DefaultHTTPPort = 9898

// ExecFunc runs a shell command inside the container hosting the pageserver and
// returns its stdout.
type ExecFunc func(ctx context.Context, script string) (string, error)

// Client is a pageserver management-API client over an in-container hop.
type Client struct {
	run  ExecFunc
	port int
}

// New returns a Client running its requests through run. A zero port means
// DefaultHTTPPort.
func New(run ExecFunc, port int) *Client {
	if port == 0 {
		port = DefaultHTTPPort
	}
	return &Client{run: run, port: port}
}

// Metrics scrapes and parses /metrics.
func (c *Client) Metrics(ctx context.Context) (Samples, error) {
	out, err := c.run(ctx, httpScript("GET", c.port, "/metrics"))
	if err != nil {
		return nil, fmt.Errorf("scrape pageserver metrics: %w", err)
	}
	return ParseSamples(out), nil
}

// FlushBranch forces the branch's buffered pages down to YugabyteDB, first
// waiting for ingest to reach lsn (a compute-side LSN such as
// pg_current_wal_flush_lsn()). Without this a test would have to wait out the
// pageserver's periodic flush interval before storage reflects its writes.
func (c *Client) FlushBranch(ctx context.Context, branchID, lsn string) error {
	path := fmt.Sprintf("/v1/branch/%s/flush?lsn=%s", branchID, lsn)
	if _, err := c.run(ctx, httpScript("PUT", c.port, path)); err != nil {
		return fmt.Errorf("force-flush branch %s at %s: %w", branchID, lsn, err)
	}
	return nil
}

// httpScript is one HTTP request over bash's /dev/tcp against the container's
// loopback: send the request, split headers from body at the first blank line,
// fail loudly on a non-2xx status (stdout would otherwise carry an error body
// that a parser happily reads as an empty exposition), and print the body.
func httpScript(method string, port int, path string) string {
	return fmt.Sprintf(`bash -c '
exec 3<>/dev/tcp/127.0.0.1/%d || exit 7
printf "%s %s HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n" >&3
resp=$(cat <&3)
status=$(printf "%%s" "$resp" | head -1)
case "$status" in
  HTTP/*" 2"*) ;;
  *) printf "%%s\n" "$status" >&2; exit 22 ;;
esac
printf "%%s" "$resp" | sed "1,/^\r\{0,1\}$/d"
'`, port, method, path)
}

// Sample is one Prometheus sample: a metric name, its labels, its value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Samples is a parsed Prometheus text exposition.
type Samples []Sample

// Value returns the value of the sample named name whose labels are a superset
// of want. Absence is reported rather than defaulted to 0: "the counter never
// moved" and "this build has no such metric" must not look alike to a caller.
func (s Samples) Value(name string, want map[string]string) (float64, bool) {
	for _, sm := range s {
		if sm.Name != name || !matches(sm.Labels, want) {
			continue
		}
		return sm.Value, true
	}
	return 0, false
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ParseSamples parses the Prometheus text exposition format, skipping comment
// lines and anything it cannot read as name{labels} value.
func ParseSamples(text string) Samples {
	var out Samples
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		head, rawValue, ok := cut(line)
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			continue
		}
		name, labels := splitLabels(head)
		out = append(out, Sample{Name: name, Labels: labels, Value: value})
	}
	return out
}

// cut splits a sample line into its series and its value at the last space, so
// a label value containing a space does not break the split.
func cut(line string) (head, value string, ok bool) {
	i := strings.LastIndex(line, " ")
	if i < 0 {
		return "", "", false
	}
	return line[:i], line[i+1:], true
}

func splitLabels(head string) (string, map[string]string) {
	open := strings.Index(head, "{")
	if open < 0 || !strings.HasSuffix(head, "}") {
		return head, nil
	}
	name := head[:open]
	labels := map[string]string{}
	for _, pair := range strings.Split(head[open+1:len(head)-1], ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return name, labels
}
