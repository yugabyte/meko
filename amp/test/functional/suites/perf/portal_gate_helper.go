package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// The portal serves history DATA (rolling median/MAD per datapoint); the band and verdict are computed here, from perf.gate.*.

const (
	DefaultGateWindow   = 10
	DefaultGateMADK     = 3.0
	DefaultGateFloorPct = 5.0
	DefaultGateMinRuns  = 3
	portalGateTimeout   = 30 * time.Second
	portalGateArtifact  = "perf-portal-gate.json"
)

type portalGateConfig struct {
	URL      string
	Window   int
	MADK     float64
	FloorPct float64
	MinRuns  int
}

func portalGateFromConfig(c *core.Config) portalGateConfig {
	return portalGateConfig{
		URL:      strings.TrimRight(c.GetString("report.portal.url", ""), "/"),
		Window:   c.GetInt("perf.gate.window", DefaultGateWindow),
		MADK:     c.GetFloat("perf.gate.mad_k", DefaultGateMADK),
		FloorPct: c.GetFloat("perf.gate.floor_pct", DefaultGateFloorPct),
		MinRuns:  c.GetInt("perf.gate.min_runs", DefaultGateMinRuns),
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

// Recomputes delta/band/verdict from the portal's data, ignoring its verdicts: band = max(mad_k x MAD%, floor%), delta signed in the worse direction.
func localGateItems(gated []core.Datapoint, items []portalGateItem, pg portalGateConfig) []portalGateItem {
	dirByName := map[string]core.Direction{}
	for _, dp := range gated {
		dirByName[dp.Name] = dp.Direction
	}
	out := make([]portalGateItem, len(items))
	for i, it := range items {
		out[i] = it
		out[i].DeltaPct = nil
		out[i].Verdict = "no_signal"
		if it.Prev.N < pg.MinRuns || it.Prev.Median <= 0 {
			continue
		}
		d, ok := regressionPct(it.Prev.Median, it.Value, dirByName[it.Name])
		if !ok {
			continue
		}
		band := math.Max(pg.MADK*(it.Prev.MAD/it.Prev.Median*100), pg.FloorPct)
		out[i].DeltaPct = &d
		out[i].Threshold = band
		out[i].ThresholdFrom = "framework"
		switch {
		case d > band:
			out[i].Verdict = "regressed"
		case d < -band:
			out[i].Verdict = "improved"
		default:
			out[i].Verdict = "neutral"
		}
	}
	return out
}

func gateCounts(items []portalGateItem) (regressed, improved, noSignal int) {
	for _, it := range items {
		switch it.Verdict {
		case "regressed":
			regressed++
		case "improved":
			improved++
		case "no_signal":
			noSignal++
		}
	}
	return
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
	verdict := "pass"
	if out.Regressed > 0 {
		verdict = "fail"
	}
	return fmt.Sprintf("perf: history gate %s for %s — %d/%d regressed, %d improved, %d without enough history (window %d, train %s)",
		verdict, testName, out.Regressed, out.Total, out.Improved, out.NoSignal, out.Window, orNone(out.YBTrain))
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
	return emitAndGateHistory(t, cfg, version, testName, gated, info, id, true, logf)
}

// assert=false stamps the history comparison but never fails on it (the live baseline arm owns the verdict).
func emitAndGateHistory(t *core.T, cfg perfConfig, version, testName string, gated, info []core.Datapoint, id Identity, assert bool, logf func(string, ...any)) error {
	pg := portalGateFromConfig(t.Options)
	if pg.URL == "" {
		if !assert {
			for _, dp := range append(gated, info...) {
				t.Datapoint(dp)
			}
			return nil
		}
		return emitAndGate(t, cfg, version, testName, gated, info, id, logf)
	}
	out, raw, err := portalGateCall(t.Context, pg, testName, version, gated, id)
	if err != nil {
		if !assert {
			logf("perf: WARN portal history unavailable (%v) — datapoints emitted without a comparison", err)
			for _, dp := range append(gated, info...) {
				t.Datapoint(dp)
			}
			return nil
		}
		logf("perf: WARN portal gate unavailable (%v) — falling back to the local baseline file", err)
		return emitAndGate(t, cfg, version, testName, gated, info, id, logf)
	}
	out.Items = localGateItems(gated, out.Items, pg)
	out.Regressed, out.Improved, out.NoSignal = gateCounts(out.Items)
	for _, dp := range applyPortalComparisons(gated, out.Items) {
		t.Datapoint(dp)
	}
	for _, dp := range info {
		t.Datapoint(dp)
	}
	t.AttachBytes(portalGateArtifact, "log", raw)
	logf("%s", portalGateSummary(testName, out))
	if assert && out.Regressed > 0 {
		return portalRegressionError(testName, out)
	}
	return nil
}
