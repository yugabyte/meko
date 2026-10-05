package mekoload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the control API of one meko-load pod.
type Client struct {
	base string
	hc   *http.Client
}

func NewClient(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) BaseURL() string { return c.base }

// APIError is a non-200 answer from the control API.
type APIError struct {
	Path    string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("meko-load %s: HTTP %d %s: %s", e.Path, e.Status, e.Code, e.Message)
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body := []byte("{}")
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("meko-load %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("meko-load %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		e := &APIError{Path: path, Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		var env struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
			e.Code, e.Message = env.Error.Code, env.Error.Message
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("meko-load %s: decode: %w", path, err)
	}
	return nil
}

type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	NodeID  string `json:"node_id"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.post(ctx, "/v1/health", nil, &h)
	return h, err
}

type Info struct {
	NowUnixMs int64 `json:"now_unix_ms"`
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.post(ctx, "/v1/info", nil, &i)
	return i, err
}

type Credential struct {
	Name  string `json:"name,omitempty"`
	Token string `json:"token"`
	// Session is the login token meko-load sends on the PAT routes, which
	// refuse a PAT on a stack with an identity provider.
	Session string `json:"session,omitempty"`
}

type TargetSpec struct {
	Name        string       `json:"name,omitempty"`
	APIURL      string       `json:"api_url,omitempty"`
	MCPURL      string       `json:"mcp_url,omitempty"`
	Credentials []Credential `json:"credentials,omitempty"`
	Timeout     string       `json:"timeout,omitempty"`
}

func (c *Client) CreateTarget(ctx context.Context, spec TargetSpec) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, "/v1/targets/create", spec, &out)
	return out.ID, err
}

func (c *Client) DeleteTarget(ctx context.Context, id string) error {
	return c.post(ctx, "/v1/targets/delete", map[string]string{"id": id}, nil)
}

func (c *Client) CreateRun(ctx context.Context, workload, target string, cfg map[string]any, labels map[string]string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.post(ctx, "/v1/runs/create", map[string]any{"workload": workload, "target": target, "config": cfg, "labels": labels}, &out)
	return out.ID, err
}

type Latency struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

type OpSummary struct {
	Count         int64            `json:"count"`
	OK            int64            `json:"ok"`
	Errors        int64            `json:"errors"`
	ErrorRate     float64          `json:"error_rate"`
	Throughput    float64          `json:"throughput"`
	OKThroughput  float64          `json:"ok_throughput"`
	ErrorsByClass map[string]int64 `json:"errors_by_class"`
	Statuses      map[string]int64 `json:"statuses"`
	Latency       Latency          `json:"latency"`
}

type Summary struct {
	ElapsedSeconds float64              `json:"elapsed_seconds"`
	Total          OpSummary            `json:"total"`
	Ops            map[string]OpSummary `json:"ops"`
	Http5xx        int64                `json:"http_5xx"`
	ErrorSamples   map[string][]string  `json:"error_samples"`
}

type Invariant struct {
	Checked  int64    `json:"checked"`
	Violated int64    `json:"violated"`
	Samples  []string `json:"samples"`
}

type ResourceCounts struct {
	DatapacksCreated int64 `json:"datapacks_created"`
	DatapacksDeleted int64 `json:"datapacks_deleted"`
	DatapacksLive    int   `json:"datapacks_live"`
	PATsCreated      int64 `json:"pats_created"`
	PATsRevoked      int64 `json:"pats_revoked"`
	PATsLive         int   `json:"pats_live"`
}

type CleanupResult struct {
	DatapacksDeleted int      `json:"datapacks_deleted"`
	DatapacksSwept   int      `json:"datapacks_swept"`
	PATsRevoked      int      `json:"pats_revoked"`
	PATsSwept        int      `json:"pats_swept"`
	LeakedDatapacks  []string `json:"leaked_datapacks"`
	LeakedPATs       []string `json:"leaked_pats"`
	Errors           []string `json:"errors"`
	Seconds          float64  `json:"seconds"`
}

// Clean is a cleanup that left nothing and hit no error.
func (c CleanupResult) Clean() bool {
	return len(c.LeakedDatapacks) == 0 && len(c.LeakedPATs) == 0 && len(c.Errors) == 0
}

// Stats is one run's runs/stats answer.
type Stats struct {
	ID         string               `json:"id"`
	State      string               `json:"state"`
	FailError  string               `json:"fail_error"`
	Summary    Summary              `json:"summary"`
	Invariants map[string]Invariant `json:"invariants"`
	Violations int64                `json:"invariant_violations"`
	Resources  ResourceCounts       `json:"resources"`
	Cleanup    *CleanupResult       `json:"cleanup"`
	HDR        map[string]string    `json:"hdr"`
}

// Finished is a run that will not change any more.
func (s Stats) Finished() bool { return s.State == "done" || s.State == "failed" }

func (c *Client) Stats(ctx context.Context, id string, withHDR bool) (Stats, error) {
	req := map[string]string{"id": id}
	if withHDR {
		req["format"] = "hdr"
	}
	var out Stats
	err := c.post(ctx, "/v1/runs/stats", req, &out)
	return out, err
}

func (c *Client) Update(ctx context.Context, id string, cfg map[string]any) error {
	return c.post(ctx, "/v1/runs/update", map[string]any{"id": id, "config": cfg}, nil)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.post(ctx, "/v1/runs/stop", map[string]string{"id": id}, nil)
}

func (c *Client) Cleanup(ctx context.Context, id string) (CleanupResult, error) {
	var out CleanupResult
	err := c.post(ctx, "/v1/runs/cleanup", map[string]string{"id": id}, &out)
	return out, err
}

func (c *Client) DeleteRun(ctx context.Context, id string) error {
	return c.post(ctx, "/v1/runs/delete", map[string]string{"id": id}, nil)
}

type LiveResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Live struct {
	Datapacks []LiveResource `json:"datapacks"`
	PATs      []LiveResource `json:"pats"`
}

func (c *Client) Resources(ctx context.Context, id string) (Live, error) {
	var out Live
	err := c.post(ctx, "/v1/runs/resources", map[string]string{"id": id}, &out)
	return out, err
}

type Bucket struct {
	Unix int64 `json:"unix"`
	OK   int64 `json:"ok"`
	Err  int64 `json:"err"`
}

type Timeline struct {
	NowUnixMs int64    `json:"now_unix_ms"`
	Buckets   []Bucket `json:"buckets"`
}

func (c *Client) Timeline(ctx context.Context, id string) (Timeline, error) {
	var out Timeline
	err := c.post(ctx, "/v1/runs/timeline", map[string]any{"id": id}, &out)
	return out, err
}

func (c *Client) Events(ctx context.Context, id string) ([]byte, error) {
	var out json.RawMessage
	err := c.post(ctx, "/v1/runs/events", map[string]string{"id": id}, &out)
	return out, err
}

// Metrics is the pod's Prometheus text.
func (c *Client) Metrics(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/metrics", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return string(b), err
}
