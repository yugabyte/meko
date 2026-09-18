package multiregion

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func loadCfg(t *testing.T, overrides ...string) *core.Config {
	t.Helper()
	cfg, err := core.LoadConfig(nil, nil, overrides)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestDefaultsAreAValidThreeRegionShape(t *testing.T) {
	c := FromConfig(loadCfg(t))
	if c.Enabled {
		t.Error("multi-region must be opt-in")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default shape invalid: %v", err)
	}
	if got := c.RegionCodes(); len(got) != 3 || got[0] != "us-west1" {
		t.Errorf("regions = %v", got)
	}
	if c.PreferredRegion != "us-west1" || c.Nodes() != 3 || c.ReplicationFactor != 3 {
		t.Errorf("shape = %+v", c)
	}
	if sec := c.SecondaryRegions(); len(sec) != 2 || sec[0] != "us-central1" {
		t.Errorf("secondary = %v", sec)
	}
}

func TestValidateRejectsBrokenShapes(t *testing.T) {
	base := FromConfig(loadCfg(t))
	one := base
	one.Regions = base.Regions[:1]
	if one.Validate() == nil {
		t.Error("one region must be rejected")
	}
	pref := base
	pref.PreferredRegion = "europe-west1"
	if pref.Validate() == nil {
		t.Error("a preferred region outside the list must be rejected")
	}
	rf := base
	rf.ReplicationFactor = 5
	if rf.Validate() == nil {
		t.Error("RF above the node count must be rejected")
	}
}

// The overlay drives a yba_provisioned GCP backing whose provider carries every
// region and whose placement puts each region's nodes in its zone.
func TestOverlayBuildsTheGCPBacking(t *testing.T) {
	cfg := loadCfg(t)
	derived := cfg.Derive(FromConfig(cfg).Overlay())
	if got := derived.GetString("env.yb_deploy", ""); got != "yba_provisioned" {
		t.Errorf("env.yb_deploy = %q", got)
	}
	if got := derived.GetString("apps.yba.provider_type", ""); got != "gcp" {
		t.Errorf("provider_type = %q", got)
	}
	spec := yba.BuildGCPUniverseSpec(derived)
	if len(spec.Placement) != 3 || spec.PreferredRegion != "us-west1" || spec.ReplicationFactor != 3 {
		t.Fatalf("universe spec = %+v", spec)
	}
	if spec.Placement[1].Code != "us-central1" || spec.Placement[1].Zones[0].Name != "us-central1-b" || spec.Placement[1].Zones[0].Nodes != 1 {
		t.Errorf("central placement = %+v", spec.Placement[1])
	}
	if spec.InstanceType != DefaultInstanceType || spec.VolumeSizeGB != DefaultVolumeSizeGB || spec.NumNodes != 3 {
		t.Errorf("spec shape = %+v", spec)
	}
	regions := derived.GetMapSlice("apps.yba.gcp.regions", nil)
	if len(regions) != 3 {
		t.Fatalf("provider regions = %v", regions)
	}
	if got := derived.GetString("apps.yba.gcp.provider_name", ""); got != DefaultProviderName {
		t.Errorf("provider_name = %q", got)
	}
	if got := derived.GetString("apps.yba.gcp.image", ""); got != DefaultImage {
		t.Errorf("image = %q", got)
	}
	// The single-region knobs the reap and the legacy path read follow the
	// preferred region.
	if derived.GetString("apps.yba.gcp.zone", "") != "us-west1-b" || derived.GetString("apps.yba.gcp.subnetwork", "") != "subnet-us-west1" {
		t.Errorf("zone/subnet = %q/%q", derived.GetString("apps.yba.gcp.zone", ""), derived.GetString("apps.yba.gcp.subnetwork", ""))
	}
}

func TestFromConfigFillsZoneAndSubnetFromTheRegion(t *testing.T) {
	cfg := loadCfg(t, `multiregion.regions=json_text:[{"code":"us-west1","nodes":2},{"code":"us-east4"}]`,
		"multiregion.replication_factor=int:3")
	c := FromConfig(cfg)
	if len(c.Regions) != 2 || c.Regions[0].Nodes != 2 || c.Regions[1].Nodes != 1 {
		t.Fatalf("regions = %+v", c.Regions)
	}
	if c.Regions[1].Zone != "us-east4-b" || c.Regions[1].Subnet != "subnet-us-east4" {
		t.Errorf("east4 = %+v", c.Regions[1])
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid shape rejected: %v", err)
	}
}

func TestResolveTarget(t *testing.T) {
	c := FromConfig(loadCfg(t))
	for _, tc := range []struct{ in, want string }{
		{"", "us-west1"}, {"preferred", "us-west1"}, {"secondary", "us-central1"}, {"us-east1", "us-east1"},
	} {
		got, err := c.ResolveTarget(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ResolveTarget(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := c.ResolveTarget("europe-west1"); err == nil {
		t.Error("an unknown region must be an error")
	}
}

func TestPreflightNamesTheMissingPiece(t *testing.T) {
	if got := Preflight(loadCfg(t)); !strings.Contains(got, "opt-in") {
		t.Errorf("disabled → %q", got)
	}
	if got := Preflight(loadCfg(t, "multiregion.enabled=bool:true")); !strings.Contains(got, "k8s cluster") {
		t.Errorf("no context → %q", got)
	}
	got := Preflight(loadCfg(t, "multiregion.enabled=bool:true", "env.sharedk8s.context=itest",
		"env.ampstack.controller_chart=/chart"))
	if !strings.Contains(got, "quay") {
		t.Errorf("no quay secret → %q", got)
	}
	got = Preflight(loadCfg(t, "multiregion.enabled=bool:true", "env.sharedk8s.context=itest",
		"env.ampstack.controller_chart=/chart", "secrets.quay_images=token:x"))
	if !strings.Contains(got, "gcp_key") {
		t.Errorf("no gcp key → %q", got)
	}
	got = Preflight(loadCfg(t, "multiregion.enabled=bool:true", "env.sharedk8s.context=itest",
		"env.ampstack.controller_chart=/chart", "secrets.quay_images=token:x", "secrets.gcp_key=token:/tmp/sa.json"))
	if got != "" {
		t.Errorf("everything present → %q, want no skip", got)
	}
}

func universeWith(nodes map[string]int, affinitized ...string) yba.Universe {
	u := yba.Universe{Name: "ampqa-mr"}
	for region, n := range nodes {
		for i := 0; i < n; i++ {
			u.UniverseDetails.NodeDetailsSet = append(u.UniverseDetails.NodeDetailsSet, yba.NodeDetails{
				NodeName: region + "-n", IsTserver: true, State: "Live", CloudInfo: yba.CloudInfo{Region: region},
			})
		}
	}
	var regions []yba.PlacementRegion
	for _, r := range affinitized {
		regions = append(regions, yba.PlacementRegion{Code: r, AZList: []yba.PlacementAZ{{Name: r + "-b", IsAffinitized: true}}})
	}
	if len(regions) > 0 {
		u.UniverseDetails.Clusters = []yba.Cluster{{PlacementInfo: yba.PlacementInfo{CloudList: []yba.PlacementCloud{{Code: "gcp", RegionList: regions}}}}}
	}
	return u
}

func TestCheckAcceptsTheShapeAndNamesEveryDeviation(t *testing.T) {
	c := FromConfig(loadCfg(t))
	ok := universeWith(map[string]int{"us-west1": 1, "us-central1": 1, "us-east1": 1}, "us-west1")
	if err := c.Check(ok); err != nil {
		t.Fatalf("matching universe rejected: %v", err)
	}
	noPlacement := universeWith(map[string]int{"us-west1": 1, "us-central1": 1, "us-east1": 1})
	if err := c.Check(noPlacement); err != nil {
		t.Errorf("a universe reporting no placement must pass the leader check: %v", err)
	}
	// A placement that affinitized NOTHING is a real finding, not the same as a
	// universe that reported no placement: a region code that never matched
	// leaves the universe with no leader preference at all.
	noAffinity := universeWith(map[string]int{"us-west1": 1, "us-central1": 1, "us-east1": 1})
	noAffinity.UniverseDetails.Clusters = []yba.Cluster{{PlacementInfo: yba.PlacementInfo{
		CloudList: []yba.PlacementCloud{{Code: "gcp", RegionList: []yba.PlacementRegion{
			{Code: "us-west1", AZList: []yba.PlacementAZ{{Name: "us-west1-b", IsAffinitized: false}}},
		}}},
	}}}
	err := c.Check(noAffinity)
	if err == nil {
		t.Fatal("a placement with no affinitized region must be reported")
	}
	if !strings.Contains(err.Error(), "no region is leader-preferred") {
		t.Errorf("error should name the missing preference: %v", err)
	}
	bad := universeWith(map[string]int{"us-west1": 2, "us-east1": 1, "europe-west1": 1}, "us-east1")
	err = c.Check(bad)
	if err == nil {
		t.Fatal("deviating universe accepted")
	}
	for _, want := range []string{"us-west1 has 2", "us-central1 has 0", "europe-west1 has 1", "leader-preferred regions are [us-east1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}
