// Package multiregion is the multi-region backing shape the multi-region tests
// share: a YugabyteDB universe spread over several GCE regions of the shared
// yugabyte-network VPC, provisioned by a YBA the framework stands up in GKE,
// with tablet leaders preferred in the region the amp control plane runs in.
// It turns the multiregion.* config into the apps.yba.gcp.* overrides a
// yba_provisioned backing reads (Overlay), gates a test on what a live run
// needs (Preflight), and checks a created universe against the shape (Check).
package multiregion

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybastack"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybdeploy"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Defaults; core/defaults_test.go asserts the embedded default.yaml agrees.
const (
	DefaultPreferredRegion   = "us-west1"
	DefaultReplicationFactor = 3
	// These tests assert topology and correctness, not throughput, so the nodes
	// are sized for a regression run rather than a benchmark: 4 vCPU / 16 GiB
	// each (the framework's own single-region GCP default) and a small disk.
	// Three regions makes every vCPU here cost three times.
	DefaultInstanceType = "n2-standard-4"
	DefaultVolumeSizeGB = 50
	// DefaultProviderName keeps the multi-region provider apart from the
	// single-region ampqa-gcp one: a provider is reused by name and its region
	// list is fixed at create.
	DefaultProviderName = "ampqa-gcp-multiregion"
	// DefaultProvisionTimeout is the whole backing bring-up: a YBA helm install,
	// the provider, the DB release import and the GCE VMs YBA provisions and
	// installs software on.
	DefaultProvisionTimeout = 90 * time.Minute
	// DefaultControllerReadyTimeout is the amp controller's own bring-up against
	// this backing. The shipped 5m is sized for a co-located database; the
	// controller's startup migrations run as cross-region DDL here.
	DefaultControllerReadyTimeout = 20 * time.Minute
	// DefaultImage is the stress framework's GCE server image (ssh user centos),
	// the one YBA is known to provision on in this project.
	DefaultImage = "projects/yugabyte/global/images/stress-server-base-x86-2025-07-31-235630"
)

// DefaultRegions is the three-region US layout: one node per region, the
// preferred one beside the GKE clusters in us-west1. Subnets follow the VPC's
// subnet-<region> naming.
var DefaultRegions = []Region{
	{Code: "us-west1", Zone: "us-west1-b", Subnet: "subnet-us-west1", Nodes: 1},
	{Code: "us-central1", Zone: "us-central1-b", Subnet: "subnet-us-central1", Nodes: 1},
	{Code: "us-east1", Zone: "us-east1-b", Subnet: "subnet-us-east1", Nodes: 1},
}

type Region struct {
	Code   string
	Zone   string
	Subnet string
	Nodes  int
}

type Config struct {
	Enabled           bool
	Regions           []Region
	PreferredRegion   string
	ReplicationFactor int
	InstanceType      string
	VolumeSizeGB      int
	Image             string
	ProviderName      string
	// ProvisionTimeout bounds standing the whole backing up: the YBA install,
	// the provider, the release import and the universe's VMs.
	ProvisionTimeout time.Duration
	// ControllerReadyTimeout bounds the amp controller becoming ready on this
	// backing. Its startup migrations are DDL, and every one of them is a
	// cross-region consensus round here rather than a local write.
	ControllerReadyTimeout time.Duration
}

type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetDuration(key string, def time.Duration) time.Duration
	GetMapSlice(key string, def []map[string]any) []map[string]any
}

func FromConfig(c Getter) Config {
	cfg := Config{
		Enabled:                c.GetBool("multiregion.enabled", false),
		PreferredRegion:        c.GetString("multiregion.preferred_region", DefaultPreferredRegion),
		ReplicationFactor:      c.GetInt("multiregion.replication_factor", DefaultReplicationFactor),
		InstanceType:           c.GetString("multiregion.instance_type", DefaultInstanceType),
		VolumeSizeGB:           c.GetInt("multiregion.volume_size_gb", DefaultVolumeSizeGB),
		Image:                  c.GetString("multiregion.image", DefaultImage),
		ProviderName:           c.GetString("multiregion.provider_name", DefaultProviderName),
		ProvisionTimeout:       c.GetDuration("multiregion.provision_timeout", DefaultProvisionTimeout),
		ControllerReadyTimeout: c.GetDuration("multiregion.controller_ready_timeout", DefaultControllerReadyTimeout),
	}
	for _, rm := range c.GetMapSlice("multiregion.regions", nil) {
		r := Region{Code: str(rm, "code"), Zone: str(rm, "zone"), Subnet: str(rm, "subnet"), Nodes: num(rm, "nodes", 1)}
		if r.Zone == "" && r.Code != "" {
			r.Zone = r.Code + "-b"
		}
		if r.Subnet == "" && r.Code != "" {
			r.Subnet = "subnet-" + r.Code
		}
		cfg.Regions = append(cfg.Regions, r)
	}
	if len(cfg.Regions) == 0 {
		cfg.Regions = append([]Region(nil), DefaultRegions...)
	}
	return cfg
}

// Validate rejects a shape a universe cannot take: fewer than two regions, a
// preferred region outside the list, or a replication factor no layout of the
// nodes can carry.
func (c Config) Validate() error {
	if len(c.Regions) < 2 {
		return fmt.Errorf("multiregion.regions lists %d region(s); a multi-region universe needs at least 2", len(c.Regions))
	}
	seen := map[string]bool{}
	nodes := 0
	for _, r := range c.Regions {
		if r.Code == "" {
			return fmt.Errorf("multiregion.regions: a region has no code")
		}
		if seen[r.Code] {
			return fmt.Errorf("multiregion.regions: region %q listed twice", r.Code)
		}
		seen[r.Code] = true
		if r.Nodes <= 0 {
			return fmt.Errorf("multiregion.regions: region %q has %d nodes", r.Code, r.Nodes)
		}
		nodes += r.Nodes
	}
	if c.PreferredRegion != "" && !seen[c.PreferredRegion] {
		return fmt.Errorf("multiregion.preferred_region %q is not one of the regions %v", c.PreferredRegion, c.RegionCodes())
	}
	if c.ReplicationFactor <= 0 || c.ReplicationFactor > nodes {
		return fmt.Errorf("multiregion.replication_factor %d needs at least that many nodes; the regions carry %d", c.ReplicationFactor, nodes)
	}
	return nil
}

func (c Config) RegionCodes() []string {
	out := make([]string, 0, len(c.Regions))
	for _, r := range c.Regions {
		out = append(out, r.Code)
	}
	return out
}

func (c Config) SecondaryRegions() []string {
	var out []string
	for _, r := range c.Regions {
		if r.Code != c.PreferredRegion {
			out = append(out, r.Code)
		}
	}
	return out
}

func (c Config) Nodes() int {
	n := 0
	for _, r := range c.Regions {
		n += r.Nodes
	}
	return n
}

// ResolveTarget maps an outage target onto a region: "preferred", "secondary"
// (the first non-preferred region) or a region code.
func (c Config) ResolveTarget(target string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "preferred":
		if c.PreferredRegion == "" {
			return "", fmt.Errorf("target %q: no preferred region configured", target)
		}
		return c.PreferredRegion, nil
	case "secondary":
		if sec := c.SecondaryRegions(); len(sec) > 0 {
			return sec[0], nil
		}
		return "", fmt.Errorf("target secondary: every region is the preferred one")
	}
	for _, r := range c.Regions {
		if r.Code == target {
			return r.Code, nil
		}
	}
	return "", fmt.Errorf("target %q is not preferred, secondary or one of %v", target, c.RegionCodes())
}

// Overlay is the config a yba_provisioned backing reads to build this shape,
// as -o overrides: the deploy mode, the gcp provider carrying every region, the
// per-region placement and the preferred region. The reap fallback zone and the
// legacy single region point at the preferred region.
func (c Config) Overlay() map[string]string {
	regions := make([]map[string]any, 0, len(c.Regions))
	placement := make([]map[string]any, 0, len(c.Regions))
	for _, r := range c.Regions {
		regions = append(regions, map[string]any{
			"code":  r.Code,
			"zones": []map[string]any{{"name": r.Zone, "subnet": r.Subnet}},
		})
		placement = append(placement, map[string]any{
			"region": r.Code,
			"zones":  []map[string]any{{"name": r.Zone, "nodes": r.Nodes}},
		})
	}
	preferred := c.preferred()
	out := map[string]string{
		"env.yb_deploy":                   ybdeploy.ModeYBAProvisioned,
		"apps.yba.provider_type":          "gcp",
		"apps.yba.gcp.provider_name":      c.ProviderName,
		"apps.yba.gcp.regions":            "json_text:" + mustJSON(regions),
		"apps.yba.gcp.placement":          "json_text:" + mustJSON(placement),
		"apps.yba.gcp.preferred_region":   c.PreferredRegion,
		"apps.yba.gcp.region":             preferred.Code,
		"apps.yba.gcp.zone":               preferred.Zone,
		"apps.yba.gcp.subnetwork":         preferred.Subnet,
		"apps.yba.gcp.num_nodes":          "int:" + strconv.Itoa(c.Nodes()),
		"apps.yba.gcp.replication_factor": "int:" + strconv.Itoa(c.ReplicationFactor),
		"apps.yba.gcp.instance_type":      c.InstanceType,
		"apps.yba.gcp.volume_size_gb":     "int:" + strconv.Itoa(c.VolumeSizeGB),
		// Standing this backing up is a YBA install plus a universe of cloud VMs
		// spread over regions, which does not fit the budget a co-located
		// backing needs.
		"timeouts.provision_dedicated": c.ProvisionTimeout.String(),
		// The controller's startup migrations are DDL, and each one costs a
		// cross-region round here rather than a local write.
		"env.ampstack.controller_ready_timeout": c.ControllerReadyTimeout.String(),
	}
	if c.Image != "" {
		out["apps.yba.gcp.image"] = c.Image
	}
	return out
}

func (c Config) preferred() Region {
	for _, r := range c.Regions {
		if r.Code == c.PreferredRegion {
			return r
		}
	}
	if len(c.Regions) > 0 {
		return c.Regions[0]
	}
	return Region{}
}

// BackingConfig is the core.EnvSpec.BackingConfig of a multi-region test: the
// overlay for the run's multiregion.* shape.
func BackingConfig(cfg *core.Config) map[string]string {
	return FromConfig(cfg).Overlay()
}

// Preflight is the skip reason a multi-region test reports before provisioning
// anything: the opt-in, a valid shape, a GKE context for the YBA and the amp
// control plane, and the secrets a live GCP universe needs. Empty means go.
func Preflight(cfg *core.Config) string {
	c := FromConfig(cfg)
	if !c.Enabled {
		return "multi-region tests are opt-in: set multiregion.enabled=bool:true (they stand up a YBA and a GCE universe across regions)"
	}
	if err := c.Validate(); err != nil {
		return err.Error()
	}
	if cfg.GetString("apps.ybastack.kubecontext", "") == "" &&
		cfg.GetString("apps.ybastack.kubeconfig", "") == "" &&
		cfg.GetString("env.ampstack.context", "") == "" &&
		cfg.GetString("env.sharedk8s.context", "") == "" {
		return "multi-region needs a k8s cluster for the YBA and the amp control plane: set env.sharedk8s.context (or apps.ybastack.kubecontext)"
	}
	if cfg.GetString("env.ampstack.controller_chart", "") == "" &&
		cfg.GetString("env.ampstack.controller_manifest", "") == "" {
		return "multi-region runs through an amp compute endpoint: set env.ampstack.controller_chart"
	}
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return "secrets not configured: " + err.Error()
	}
	quay := cfg.GetString("apps.ybastack.quay_secret", ybastack.DefaultQuaySecret)
	if _, err := secrets.Value(quay); err != nil {
		return fmt.Sprintf("quay pull secret %q not resolvable: declare it under secrets:", quay)
	}
	creds := cfg.GetString("apps.yba.gcp.credentials_secret", yba.DefaultGCPCredentialsSecret)
	if _, err := secrets.Value(creds); err != nil {
		return fmt.Sprintf("GCP service-account key %q not resolvable: declare it under secrets: (its value is the PATH to the SA JSON)", creds)
	}
	return ""
}

// Check compares a created universe against the shape: every configured region
// carries exactly its node count, no other region carries any, and when a
// region is preferred it is the only one YBA affinitized.
func (c Config) Check(u yba.Universe) error {
	got := u.RegionNodes()
	var problems []string
	for _, r := range c.Regions {
		if n := len(got[r.Code]); n != r.Nodes {
			problems = append(problems, fmt.Sprintf("region %s has %d tserver(s), want %d", r.Code, n, r.Nodes))
		}
		delete(got, r.Code)
	}
	for region, nodes := range got {
		problems = append(problems, fmt.Sprintf("region %s has %d tserver(s) the shape never asked for", region, len(nodes)))
	}
	// Only checked once the universe reports a placement at all: a universe that
	// carries none has nothing to disagree with. When it does carry one, an
	// EMPTY affinity set is a real finding — a region code that never matched
	// leaves every zone un-preferred, which is not the shape that was asked for.
	if c.PreferredRegion != "" && u.HasPlacement() {
		switch aff := u.AffinitizedRegions(); {
		case len(aff) == 0:
			problems = append(problems, fmt.Sprintf("no region is leader-preferred, want %s", c.PreferredRegion))
		case len(aff) != 1 || aff[0] != c.PreferredRegion:
			problems = append(problems, fmt.Sprintf("leader-preferred regions are %v, want only %s", aff, c.PreferredRegion))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("universe %q does not match the multi-region shape: %s", u.Name, strings.Join(problems, "; "))
	}
	return nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func str(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func num(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
