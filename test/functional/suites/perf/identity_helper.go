package perf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/baseline"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Result identity is what makes a perf number comparable. A throughput figure is
// meaningless without knowing WHAT was measured (YB version/image, amp images,
// framework commit) and — critically — at what LOAD. Two results are comparable
// iff their load_signature matches (an identical load config); the identity
// fields then say which thing-under-test differed. Both the single-run gate
// (comparability check vs the stored baseline) and the A/B comparison rely on
// this: the comparison holds the load constant across variants, and the gate
// refuses to compare across different loads.

// Identity is the self-describing provenance of one perf result.
type Identity struct {
	Variant            string // comparison variant label; "" for a single run
	YBVersion          string
	YBImage            string // registry:tag as resolved
	AmpControllerImage string
	AmpEndpointImage   string
	Commit             string // framework rev, best-effort ("" when unavailable)
	LoadSignature      string // deterministic hash of the load config (see LoadFields)
}

// LoadFields is the load configuration that MUST be identical for two results to
// be comparable. Its deterministic hash is the LoadSignature. A workload fills
// the fields its shape uses (tpcc: warehouses; pgbench: scale/clients/jobs;
// sysbench: script/tables + scale/clients) and leaves the rest zero — the
// signature still differs whenever any tracked knob changes.
type LoadFields struct {
	Iterations    int    `json:"iterations"`
	WarmupMS      int64  `json:"warmup_ms"`
	MeasurementMS int64  `json:"measurement_ms"`
	Scale         int    `json:"scale"`
	Clients       int    `json:"clients"`
	Jobs          int    `json:"jobs"`
	RF            int    `json:"rf"`
	Warehouses    int    `json:"warehouses"`
	Tables        int    `json:"tables"`
	Script        string `json:"script"`
}

// LoadSignature is a deterministic short hash of the load config. Encoding a
// struct (fixed field order, no maps) makes it stable; two LoadFields that
// differ in any field hash differently, and two identical ones hash the same.
func LoadSignature(f LoadFields) string {
	b, _ := json.Marshal(f)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// loadFieldsTPCC is the perf/tpcc load: the shared bench cadence plus RF and the
// TPC-C warehouse scalefactor.
func loadFieldsTPCC(bc BenchConfig, cfg perfConfig) LoadFields {
	return LoadFields{
		Iterations:    bc.Iterations,
		WarmupMS:      bc.Warmup.Milliseconds(),
		MeasurementMS: bc.MeasurementDuration.Milliseconds(),
		RF:            cfg.RF,
		Warehouses:    cfg.Warehouses,
	}
}

// loadFieldsPgbench is the perf/pgbench load: the shared bench cadence plus RF
// and the pgbench scale/clients/jobs shape.
func loadFieldsPgbench(bc BenchConfig, cfg perfConfig, params pgbenchParams) LoadFields {
	return LoadFields{
		Iterations:    bc.Iterations,
		WarmupMS:      bc.Warmup.Milliseconds(),
		MeasurementMS: bc.MeasurementDuration.Milliseconds(),
		Scale:         params.Scale,
		Clients:       params.Clients,
		Jobs:          params.Jobs,
		RF:            cfg.RF,
	}
}

// buildIdentity assembles the identity from a resolved ybk8s Config and the amp
// image refs, the framework commit, and the load signature. Pure — commit and
// loadSig are injected so it is deterministic under test.
func buildIdentity(variant string, ybCfg ybk8s.Config, ampCtrl, ampEndpoint, commit, loadSig string) Identity {
	return Identity{
		Variant:            variant,
		YBVersion:          ybCfg.ChartVersion,
		YBImage:            ybImageRef(ybCfg),
		AmpControllerImage: ampCtrl,
		AmpEndpointImage:   ampEndpoint,
		Commit:             commit,
		LoadSignature:      loadSig,
	}
}

// identityFromConfig resolves the single-run identity from a config: the ybk8s
// image/version and the configured amp images (recorded even though the perf
// path benchmarks the backing directly — the identity is self-describing).
func identityFromConfig(c *core.Config, variant, commit, loadSig string) Identity {
	amp := ampstack.FromConfig(c)
	return buildIdentity(variant, ybk8s.FromConfig(c), amp.ControllerImage, amp.EndpointImage, commit, loadSig)
}

// ybImageRef is the resolved "registry:tag" YB image, or "" when neither is set.
func ybImageRef(c ybk8s.Config) string {
	if c.ImageRegistry == "" && c.ImageTag == "" {
		return ""
	}
	return c.ImageRegistry + ":" + c.ImageTag
}

// core converts an Identity into the report-shape core.PerfIdentity.
func (id Identity) core() core.PerfIdentity {
	return core.PerfIdentity{
		Variant:            id.Variant,
		YBVersion:          id.YBVersion,
		YBImage:            id.YBImage,
		AmpControllerImage: id.AmpControllerImage,
		AmpEndpointImage:   id.AmpEndpointImage,
		Commit:             id.Commit,
		LoadSignature:      id.LoadSignature,
	}
}

// meta is the identity in the baseline store's shape (the comparability key +
// image/commit for the mismatch WARN).
func (id Identity) meta() baseline.Meta {
	return baseline.Meta{LoadSignature: id.LoadSignature, YBImage: id.YBImage, Commit: id.Commit}
}

// frameworkCommit best-effort resolves the framework git revision (short SHA).
// It is guarded and non-fatal: no git repo, no git binary, or any error yields
// "" so a perf run never depends on git. Used only in live wiring — unit tests
// inject a fixed commit.
func frameworkCommit() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
