package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// The portal holds the only perf history that survives a CI workspace.

const (
	DefaultGateWindow  = 10
	portalGateTimeout  = 30 * time.Second
	portalGateArtifact = "perf-portal-gate.json"
)

type portalGateConfig struct {
	URL    string
	Window int
	MADK   float64
}

func portalGateFromConfig(c *core.Config) portalGateConfig {
	return portalGateConfig{
		URL:    strings.TrimRight(c.GetString("report.portal.url", ""), "/"),
		Window: c.GetInt("perf.gate.window", DefaultGateWindow),
		MADK:   c.GetFloat("perf.gate.mad_k", 0),
	}
}

type portalGatePrev struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	MAD    float64 `json:"mad"`
}

type portalGateItem struct {
	Name          string         `json:"name"`
	Value         float64        `json:"value"`
	Unit          string         `json:"unit"`
	Prev          portalGatePrev `json:"prev"`
	DeltaPct      *float64       `json:"delta_pct"`
	Threshold     float64        `json:"threshold"`
	ThresholdFrom string         `json:"threshold_from"`
	Verdict       string         `json:"verdict"`
}

type portalGateResponse struct {
	Gate      string           `json:"gate"`
	Window    int              `json:"window"`
	MADK      float64          `json:"mad_k"`
	YBTrain   string           `json:"yb_train"`
	Regressed int              `json:"regressed"`
	Improved  int              `json:"improved"`
	NoSignal  int              `json:"no_signal"`
	Total     int              `json:"datapoints_total"`
	Items     []portalGateItem `json:"items"`
}

func portalGateCall(ctx context.Context, pg portalGateConfig, testName, ybVersion string, gated []core.Datapoint, id Identity) (*portalGateResponse, []byte, error) {
	dps := make([]map[string]any, 0, len(gated))
	for _, dp := range gated {
		dps = append(dps, map[string]any{
			"name": dp.Name, "value": dp.Value, "unit": dp.Unit,
			"direction": string(dp.Direction), "load_signature": id.LoadSignature,
		})
	}
	body, err := json.Marshal(map[string]any{
		"test": testName, "yb_version": ybVersion,
		"window": pg.Window, "mad_k": pg.MADK, "datapoints": dps,
	})
	if err != nil {
		return nil, nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, portalGateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		pg.URL+"/api/v1/perf/gate", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("portal gate: HTTP %d: %s", resp.StatusCode, tail(buf.String(), 200))
	}
	var out portalGateResponse
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, nil, fmt.Errorf("portal gate: decode response: %w", err)
	}
	return &out, buf.Bytes(), nil
}

func applyPortalComparisons(gated []core.Datapoint, items []portalGateItem) []core.Datapoint {
	byName := map[string]portalGateItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	out := make([]core.Datapoint, len(gated))
	for i, dp := range gated {
		out[i] = dp
		it, ok := byName[dp.Name]
		if !ok || it.DeltaPct == nil {
			continue
		}
		out[i].Baseline = &core.BaselineComparison{
			Value:     it.Prev.Median,
			DeltaPct:  *it.DeltaPct,
			Threshold: it.Threshold,
			Regressed: it.Verdict == "regressed",
		}
	}
	return out
}

func portalGateSummary(testName string, out *portalGateResponse) string {
	return fmt.Sprintf("perf: portal gate %s for %s — %d/%d regressed, %d improved, %d without enough history (window %d, train %s)",
		out.Gate, testName, out.Regressed, out.Total, out.Improved, out.NoSignal, out.Window, orNone(out.YBTrain))
}

func portalRegressionError(testName string, out *portalGateResponse) error {
	var parts []string
	for _, it := range out.Items {
		if it.Verdict != "regressed" || it.DeltaPct == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %+.1f%% vs median %s of %d runs (band ±%.1f%% from %s)",
			it.Name, *it.DeltaPct, trimFloatStr(it.Prev.Median), it.Prev.N, it.Threshold, it.ThresholdFrom))
	}
	return fmt.Errorf("perf regression vs portal history for %s: %s", testName, strings.Join(parts, "; "))
}

func trimFloatStr(v float64) string {
	s := fmt.Sprintf("%.3f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func emitAndGateAuto(t *core.T, cfg perfConfig, version, testName string, gated, info []core.Datapoint, id Identity, logf func(string, ...any)) error {
	pg := portalGateFromConfig(t.Options)
	if pg.URL == "" {
		return emitAndGate(t, cfg, version, testName, gated, info, id, logf)
	}
	out, raw, err := portalGateCall(t.Context, pg, testName, version, gated, id)
	if err != nil {
		logf("perf: WARN portal gate unavailable (%v) — falling back to the local baseline file", err)
		return emitAndGate(t, cfg, version, testName, gated, info, id, logf)
	}
	for _, dp := range applyPortalComparisons(gated, out.Items) {
		t.Datapoint(dp)
	}
	for _, dp := range info {
		t.Datapoint(dp)
	}
	t.AttachBytes(portalGateArtifact, "log", raw)
	logf("%s", portalGateSummary(testName, out))
	if out.Gate == "fail" {
		return portalRegressionError(testName, out)
	}
	return nil
}
