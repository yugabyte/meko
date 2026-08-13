// Package pageserver drives the pageserver's management HTTP API from a
// test: scraping its Prometheus metrics, reading a branch's LSN state
// (including the applied GC cutoff), forcing a flush of a branch's buffered
// pages down to YugabyteDB, and forcing GC to run now.
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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// DefaultHTTPPort is the pageserver's management HTTP port (PAGESERVER_HTTP_PORT).
const DefaultHTTPPort = 9898

// ConnectRefusedExit is the exit code a request script uses when nothing accepts
// a connection on the management port, so a caller can tell "no pageserver to
// talk to here" (an environment shape) from an error the pageserver returned.
const ConnectRefusedExit = 7

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

// BranchInfo is the part of a branch's pageserver detail the tests read.
// AppliedGCCutoffLSN is how far GC has advanced: history below it is reclaimed,
// so a fork point below it has nothing left to read.
type BranchInfo struct {
	BranchID           string `json:"branch_id"`
	LastRecordLSN      string `json:"last_record_lsn"`
	AppliedGCCutoffLSN string `json:"applied_gc_cutoff_lsn"`
	MinReadableLSN     string `json:"min_readable_lsn"`
}

// GCResult is what a forced GC run reports it reclaimed.
type GCResult struct {
	CandidatesDequeued uint64 `json:"candidates_dequeued"`
	RowsDeleted        uint64 `json:"rows_deleted"`
	ElapsedMS          uint64 `json:"elapsed"`
}

// BranchInfo reads the branch's LSN state.
func (c *Client) BranchInfo(ctx context.Context, branchID string) (BranchInfo, error) {
	out, err := c.run(ctx, httpScript("GET", c.port, "/v1/branch/"+branchID))
	if err != nil {
		return BranchInfo{}, fmt.Errorf("read pageserver detail of branch %s: %w", branchID, err)
	}
	var info BranchInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return BranchInfo{}, fmt.Errorf("parse pageserver detail of branch %s (%q): %w", branchID, clip(out), err)
	}
	return info, nil
}

// ForceGC runs the branch's GC now instead of waiting out gc_period, with
// gcHorizonBytes as this run's space-based retention. The branch's pitr_interval
// still applies and is authoritative over the horizon when it can be resolved.
func (c *Client) ForceGC(ctx context.Context, branchID string, gcHorizonBytes uint64) (GCResult, error) {
	body := fmt.Sprintf(`{"gc_horizon":%d}`, gcHorizonBytes)
	out, err := c.run(ctx, httpScriptBody("PUT", c.port, "/v1/branch/"+branchID+"/do_gc", body))
	if err != nil {
		return GCResult{}, fmt.Errorf("force GC on branch %s at gc_horizon %d bytes: %w", branchID, gcHorizonBytes, err)
	}
	var res GCResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return GCResult{}, fmt.Errorf("parse forced-GC result for branch %s (%q): %w", branchID, clip(out), err)
	}
	return res, nil
}

// ParseLSN reads Postgres's hex "hi/lo" LSN rendering into its 64-bit value, so
// LSNs from different surfaces (a SQL query, this API) can be compared.
func ParseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fmt.Errorf("malformed LSN %q: want hex hi/lo", s)
	}
	high, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("malformed LSN %q: %w", s, err)
	}
	low, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("malformed LSN %q: %w", s, err)
	}
	return high<<32 | low, nil
}

// clip bounds an unparseable response quoted into an error message.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// httpScript is a request with no body.
func httpScript(method string, port int, path string) string {
	return httpScriptBody(method, port, path, "")
}

// httpScriptBody is one HTTP request over bash's /dev/tcp against the container's
// loopback: send the request, split headers from body at the first blank line,
// fail loudly on a non-2xx status carrying both the status line and the body
// (stdout would otherwise carry an error body that a parser happily reads as an
// empty exposition), and print the body. A JSON body must contain no % or single
// quote: either would break the shell printf that sends it.
func httpScriptBody(method string, port int, path, body string) string {
	req := fmt.Sprintf(`%s %s HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n`, method, path)
	if body == "" {
		req += `\r\n`
	} else {
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(body)
		req += fmt.Sprintf(`Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s`, len(body), escaped)
	}
	return fmt.Sprintf(`bash -c '
exec 3<>/dev/tcp/127.0.0.1/%d || exit %d
printf "%s" >&3
resp=$(cat <&3)
status=$(printf "%%s" "$resp" | head -1)
body=$(printf "%%s" "$resp" | sed "1,/^\r\{0,1\}$/d")
case "$status" in
  HTTP/*" 2"*) ;;
  *) printf "%%s: %%s\n" "$status" "$body" >&2; exit 22 ;;
esac
printf "%%s" "$body"
'`, port, ConnectRefusedExit, req)
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
