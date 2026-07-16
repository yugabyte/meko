// Package yba is a minimal, read-only YugabyteDB Anywhere (YBA) REST client
// used by the admin console to surface live universe configuration and
// metrics. It authenticates with a single API token (the X-AUTH-YW-API-TOKEN
// header) and resolves the customer UUID from /session_info on first use, so
// the only secret the deployment supplies is the token itself — the universe's
// management URL and UUID come from meko_system.yb_universes.
package yba

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const tokenHeader = "X-AUTH-YW-API-TOKEN"

// Client talks to one YBA install. It is safe for concurrent use. A nil
// *Client means YBA is not configured (no token); callers must nil-check.
type Client struct {
	token string
	http  *http.Client

	mu    sync.Mutex
	cUUID string // customer UUID, resolved lazily from /session_info and cached
}

// NewClient returns a YBA client for the given API token, or nil when the
// token is empty (YBA integration disabled — the admin console then shows the
// data plane as "not configured").
func NewClient(token string) *Client {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return &Client{token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// Universe is the subset of a YBA universe's configuration the admin console
// renders. Handles both cloud (VM, instanceType) and Kubernetes (per-process
// resource specs) providers. Region names aren't in the user-intent — regionList
// there holds region UUIDs — so Region is derived from the live node set.
type Universe struct {
	Name              string
	Provider          string
	SoftwareVersion   string
	InstanceType      string  // cloud providers; empty for kubernetes
	ReplicationFactor int
	NumNodes          int     // logical node count (userIntent.numNodes)
	TotalNodes        int     // node/pod entries in nodeDetailsSet
	LiveNodes         int     // entries in the "Live" state
	Region            string
	VolumeSizeGB      int
	NumVolumes        int
	// Per-process resource specs (Kubernetes). Zero for cloud VM providers,
	// which describe capacity via InstanceType instead.
	TServerCores      float64
	TServerMemoryGiB  float64
	MasterCores       float64
	MasterMemoryGiB   float64
	NodePrefix        string
}

// GetUniverse fetches a universe's configuration:
// GET /api/v1/customers/{cUUID}/universes/{universeUUID}.
func (c *Client) GetUniverse(ctx context.Context, mgmtURL, universeUUID string) (*Universe, error) {
	cUUID, err := c.customerUUID(ctx, mgmtURL)
	if err != nil {
		return nil, err
	}

	type k8sSpec struct {
		MemoryGib    float64 `json:"memoryGib"`
		CPUCoreCount float64 `json:"cpuCoreCount"`
	}
	var raw struct {
		Name            string `json:"name"`
		UniverseDetails struct {
			NodePrefix string `json:"nodePrefix"`
			Clusters   []struct {
				ClusterType string `json:"clusterType"`
				UserIntent  struct {
					NumNodes          int      `json:"numNodes"`
					ReplicationFactor int      `json:"replicationFactor"`
					InstanceType      string   `json:"instanceType"`
					YBSoftwareVersion string   `json:"ybSoftwareVersion"`
					ProviderType      string   `json:"providerType"`
					TServerK8SSpec    *k8sSpec `json:"tserverK8SNodeResourceSpec"`
					MasterK8SSpec     *k8sSpec `json:"masterK8SNodeResourceSpec"`
					DeviceInfo        struct {
						NumVolumes int `json:"numVolumes"`
						VolumeSize int `json:"volumeSize"`
					} `json:"deviceInfo"`
				} `json:"userIntent"`
				// Resolved region metadata (sibling of userIntent) — this is
				// where human-readable region names live; userIntent.regionList
				// only has UUIDs.
				Regions []struct {
					Code string `json:"code"`
					Name string `json:"name"`
				} `json:"regions"`
			} `json:"clusters"`
			NodeDetailsSet []struct {
				State     string `json:"state"`
				CloudInfo struct {
					Region string `json:"region"`
				} `json:"cloudInfo"`
			} `json:"nodeDetailsSet"`
		} `json:"universeDetails"`
	}

	url := fmt.Sprintf("%s/api/v1/customers/%s/universes/%s", trimURL(mgmtURL), cUUID, universeUUID)
	if err := c.do(ctx, http.MethodGet, url, nil, &raw); err != nil {
		return nil, err
	}

	u := &Universe{Name: raw.Name, NodePrefix: raw.UniverseDetails.NodePrefix}
	regionNames := map[string]struct{}{}
	// Prefer the PRIMARY cluster; fall back to the first cluster present.
	for i, cl := range raw.UniverseDetails.Clusters {
		if cl.ClusterType != "PRIMARY" && i != 0 {
			continue
		}
		ui := cl.UserIntent
		u.NumNodes = ui.NumNodes
		u.ReplicationFactor = ui.ReplicationFactor
		u.InstanceType = ui.InstanceType
		u.SoftwareVersion = ui.YBSoftwareVersion
		u.Provider = ui.ProviderType
		u.NumVolumes = ui.DeviceInfo.NumVolumes
		u.VolumeSizeGB = ui.DeviceInfo.VolumeSize
		// Cloud providers describe capacity via instanceType; Kubernetes gives
		// per-process resource specs. Capture both tserver (data) and master.
		if spec := ui.TServerK8SSpec; spec != nil {
			u.TServerCores, u.TServerMemoryGiB = spec.CPUCoreCount, spec.MemoryGib
		}
		if spec := ui.MasterK8SSpec; spec != nil {
			u.MasterCores, u.MasterMemoryGiB = spec.CPUCoreCount, spec.MemoryGib
		}
		// Human-readable region names (fall back to the region code).
		for _, rg := range cl.Regions {
			if name := strings.TrimSpace(rg.Name); name != "" {
				regionNames[name] = struct{}{}
			} else if code := strings.TrimSpace(rg.Code); code != "" {
				regionNames[code] = struct{}{}
			}
		}
		if cl.ClusterType == "PRIMARY" {
			break
		}
	}

	nodeRegions := map[string]struct{}{}
	for _, n := range raw.UniverseDetails.NodeDetailsSet {
		u.TotalNodes++
		if strings.EqualFold(n.State, "Live") {
			u.LiveNodes++
		}
		if r := strings.TrimSpace(n.CloudInfo.Region); r != "" {
			nodeRegions[r] = struct{}{}
		}
	}
	// Prefer resolved region names from the cluster; fall back to node-level
	// region codes (e.g. "us-east-2") if the cluster carried none.
	if len(regionNames) > 0 {
		u.Region = joinKeys(regionNames)
	} else {
		u.Region = joinKeys(nodeRegions)
	}
	return u, nil
}

// GetMetrics fetches the latest value of each named metric for a universe:
// POST /api/v1/customers/{cUUID}/metrics over the last `window`. Best-effort —
// YBA's metric graph payload varies by version, so a metric that can't be
// parsed is simply omitted from the returned map rather than erroring.
func (c *Client) GetMetrics(
	ctx context.Context, mgmtURL, universeUUID, nodePrefix string, names []string, window time.Duration,
) (map[string]float64, error) {
	cUUID, err := c.customerUUID(ctx, mgmtURL)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	body := map[string]any{
		"metrics":      names,
		"start":        fmt.Sprintf("%d", now.Add(-window).Unix()),
		"end":          fmt.Sprintf("%d", now.Unix()),
		"universeUuid": universeUUID,
		"nodePrefix":   nodePrefix,
	}

	// Each metric maps to a graph with one or more node series; y holds the
	// time-ordered values. We average the last value across series.
	var raw map[string]struct {
		Data []struct {
			Y []json.Number `json:"y"`
		} `json:"data"`
	}
	url := fmt.Sprintf("%s/api/v1/customers/%s/metrics", trimURL(mgmtURL), cUUID)
	if err := c.do(ctx, http.MethodPost, url, body, &raw); err != nil {
		return nil, err
	}

	out := map[string]float64{}
	for _, name := range names {
		graph, ok := raw[name]
		if !ok || len(graph.Data) == 0 {
			continue
		}
		var sum float64
		var n int
		for _, series := range graph.Data {
			if len(series.Y) == 0 {
				continue
			}
			if v, err := series.Y[len(series.Y)-1].Float64(); err == nil {
				sum += v
				n++
			}
		}
		if n > 0 {
			out[name] = sum / float64(n)
		}
	}
	return out, nil
}

// customerUUID resolves and caches the customer UUID from /session_info.
func (c *Client) customerUUID(ctx context.Context, mgmtURL string) (string, error) {
	c.mu.Lock()
	cached := c.cUUID
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	var si struct {
		CustomerUUID string `json:"customerUUID"`
	}
	if err := c.do(ctx, http.MethodGet, trimURL(mgmtURL)+"/api/v1/session_info", nil, &si); err != nil {
		return "", err
	}
	if si.CustomerUUID == "" {
		return "", fmt.Errorf("yba session_info returned no customerUUID")
	}

	c.mu.Lock()
	c.cUUID = si.CustomerUUID
	c.mu.Unlock()
	return si.CustomerUUID, nil
}

// do issues a JSON request with the token header and decodes a 2xx body.
func (c *Client) do(ctx context.Context, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set(tokenHeader, c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("yba %s %s: status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("yba decode %s: %w", url, err)
		}
	}
	return nil
}

func trimURL(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

func joinKeys(set map[string]struct{}) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	// Deterministic order without importing sort for such a small set.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ", ")
}
