package yba

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func multiRegionSpec() UniverseSpec {
	return UniverseSpec{
		NamePrefix:        "mekoqa",
		RunTag:            "mr1",
		ProviderUUID:      "prov-g",
		ProviderCode:      "gcp",
		InstanceType:      "n2-standard-8",
		ReplicationFactor: 3,
		YBSoftwareVersion: "2025.1.0.0-b1",
		VolumeSizeGB:      250,
		VolumeCount:       1,
		PreferredRegion:   "us-west1",
		Placement: []RegionPlacement{
			{Code: "us-west1", UUID: "r-west", Zones: []ZonePlacement{{Name: "us-west1-b", UUID: "z-west", Subnet: "subnet-us-west1", Nodes: 1}}},
			{Code: "us-central1", UUID: "r-central", Zones: []ZonePlacement{{Name: "us-central1-b", UUID: "z-central", Nodes: 1}}},
			{Code: "us-east1", UUID: "r-east", Zones: []ZonePlacement{{Name: "us-east1-b", UUID: "z-east", Nodes: 1}}},
		},
	}
}

// The placement shapes regionList and numNodes; placementInfo is YBA's to
// generate, so the create body must NOT carry a hand-built one.
func TestBuildConfigureRequestPlacementShapesRegionsOnly(t *testing.T) {
	req := BuildConfigureRequest(multiRegionSpec())
	cluster := req["clusters"].([]any)[0].(map[string]any)
	intent := cluster["userIntent"].(map[string]any)

	if got := intent["numNodes"]; got != 3 {
		t.Errorf("numNodes = %v, want 3 (sum of the placement)", got)
	}
	if got := intent["regionList"].([]any); len(got) != 3 || got[0] != "r-west" || got[2] != "r-east" {
		t.Errorf("regionList = %v", got)
	}
	if _, has := cluster["placementInfo"]; has {
		t.Error("the create body must not hand-build placementInfo, YBA generates it")
	}
}

// Without a placement nothing about the request changes.
func TestBuildConfigureRequestNoPlacementUnchanged(t *testing.T) {
	s := multiRegionSpec()
	s.Placement, s.PreferredRegion = nil, ""
	s.NumNodes, s.RegionUUIDs = 1, []string{"r-west"}
	cluster := BuildConfigureRequest(s)["clusters"].([]any)[0].(map[string]any)
	intent := cluster["userIntent"].(map[string]any)
	if intent["numNodes"] != 1 {
		t.Errorf("numNodes = %v", intent["numNodes"])
	}
	if got := intent["regionList"].([]any); len(got) != 1 || got[0] != "r-west" {
		t.Errorf("regionList = %v, want the single-region list", got)
	}
}

// configuredBody is what /universe_configure hands back: a generated placement
// with every zone affinitized and YBA's own even node spread.
func configuredBody() map[string]any {
	az := func(name string, n int) map[string]any {
		return map[string]any{"name": name, "numNodesInAZ": n, "replicationFactor": 1, "isAffinitized": true}
	}
	return map[string]any{"clusters": []any{map[string]any{
		"clusterType": "PRIMARY",
		"placementInfo": map[string]any{"cloudList": []any{map[string]any{
			"code": "gcp",
			"regionList": []any{
				map[string]any{"uuid": "r-west", "code": "us-west1", "azList": []any{az("us-west1-b", 1)}},
				map[string]any{"uuid": "r-central", "code": "us-central1", "azList": []any{az("us-central1-b", 1)}},
				map[string]any{"uuid": "r-east", "code": "us-east1", "azList": []any{az("us-east1-b", 1)}},
			},
		}}},
	}}}
}

// Only the preferred region's zones stay affinitized, and the cloud entry names
// it as the default region.
func TestApplyPlacementPreferenceMarksOnlyThePreferredRegion(t *testing.T) {
	body := configuredBody()
	applyPlacementPreference(body, multiRegionSpec())

	cloud := body["clusters"].([]any)[0].(map[string]any)["placementInfo"].(map[string]any)["cloudList"].([]any)[0].(map[string]any)
	// defaultRegion is the geo-partitioning knob, YBA puts every master in it,
	// which an RF3 universe with one node per region cannot satisfy.
	if _, has := cloud["defaultRegion"]; has {
		t.Errorf("defaultRegion must not be set for leader preference: %v", cloud["defaultRegion"])
	}
	for _, r := range cloud["regionList"].([]any) {
		region := r.(map[string]any)
		want := region["code"] == "us-west1"
		for _, a := range region["azList"].([]any) {
			if got := a.(map[string]any)["isAffinitized"]; got != want {
				t.Errorf("region %v isAffinitized = %v, want %v", region["code"], got, want)
			}
		}
	}
}

// No preferred region leaves every zone affinitized and names no default.
func TestApplyPlacementPreferenceNoPreferredRegion(t *testing.T) {
	s := multiRegionSpec()
	s.PreferredRegion = ""
	body := configuredBody()
	applyPlacementPreference(body, s)

	cloud := body["clusters"].([]any)[0].(map[string]any)["placementInfo"].(map[string]any)["cloudList"].([]any)[0].(map[string]any)
	if _, has := cloud["defaultRegion"]; has {
		t.Error("defaultRegion emitted without a preferred region")
	}
	for _, r := range cloud["regionList"].([]any) {
		for _, a := range r.(map[string]any)["azList"].([]any) {
			if a.(map[string]any)["isAffinitized"] != true {
				t.Errorf("zone not affinitized: %v", a)
			}
		}
	}
}

// An uneven layout overrides YBA's even spread on the zones it names.
func TestApplyPlacementPreferenceOverridesNodeCounts(t *testing.T) {
	s := multiRegionSpec()
	s.Placement[0].Zones[0].Nodes = 3
	body := configuredBody()
	applyPlacementPreference(body, s)

	regions := body["clusters"].([]any)[0].(map[string]any)["placementInfo"].(map[string]any)["cloudList"].([]any)[0].(map[string]any)["regionList"].([]any)
	if got := regions[0].(map[string]any)["azList"].([]any)[0].(map[string]any)["numNodesInAZ"]; got != 3 {
		t.Errorf("us-west1 numNodesInAZ = %v, want 3", got)
	}
	if got := regions[1].(map[string]any)["azList"].([]any)[0].(map[string]any)["numNodesInAZ"]; got != 1 {
		t.Errorf("us-central1 numNodesInAZ = %v, want YBA's 1", got)
	}
}

func TestResolvePlacementFillsUUIDsAndSubnets(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/providers/prov-g/regions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"uuid": "r-west", "code": "us-west1", "zones": []map[string]any{{"uuid": "z-west", "code": "us-west1-b", "name": "us-west1-b", "subnet": "subnet-us-west1"}}},
			{"uuid": "r-east", "code": "us-east1", "zones": []map[string]any{{"uuid": "z-east", "code": "us-east1-b", "name": "us-east1-b", "subnet": "subnet-us-east1"}}},
		})
	})
	got, err := f.client().ResolvePlacement(context.Background(), "prov-g", []RegionPlacement{
		{Code: "us-west1", Zones: []ZonePlacement{{Name: "us-west1-b", Nodes: 1}}},
		{Code: "us-east1", Zones: []ZonePlacement{{Name: "us-east1-b", Nodes: 2}}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].UUID != "r-west" || got[0].Zones[0].UUID != "z-west" || got[0].Zones[0].Subnet != "subnet-us-west1" {
		t.Errorf("west = %+v", got[0])
	}
	if got[1].UUID != "r-east" || got[1].Zones[0].UUID != "z-east" || got[1].Zones[0].Nodes != 2 {
		t.Errorf("east = %+v", got[1])
	}

	_, err = f.client().ResolvePlacement(context.Background(), "prov-g", []RegionPlacement{
		{Code: "us-central1", Zones: []ZonePlacement{{Name: "us-central1-b", Nodes: 1}}},
	})
	if err == nil {
		t.Errorf("a region the provider lacks must be an error")
	}
	_, err = f.client().ResolvePlacement(context.Background(), "prov-g", []RegionPlacement{
		{Code: "us-west1", Zones: []ZonePlacement{{Name: "us-west1-z", Nodes: 1}}},
	})
	if err == nil {
		t.Errorf("a zone the provider lacks must be an error")
	}
}

func TestReadPlacementFromConfig(t *testing.T) {
	c := fakeConfig{m: map[string]any{
		"apps.yba.gcp.placement": []map[string]any{
			{"region": "us-west1", "zones": []any{map[string]any{"name": "us-west1-b", "nodes": 2}}},
			{"code": "us-east1", "zones": []any{map[string]any{"name": "us-east1-b"}}},
		},
		"apps.yba.gcp.preferred_region": "us-west1",
	}}
	spec := BuildGCPUniverseSpec(c)
	if len(spec.Placement) != 2 || spec.PreferredRegion != "us-west1" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Placement[0].Zones[0].Nodes != 2 {
		t.Errorf("west zone = %+v", spec.Placement[0].Zones[0])
	}
	if spec.Placement[1].Code != "us-east1" || spec.Placement[1].Zones[0].Nodes != 1 {
		t.Errorf("east = %+v (nodes default to 1)", spec.Placement[1])
	}
	if got := spec.PlacementRegionCodes(); len(got) != 2 || got[1] != "us-east1" {
		t.Errorf("codes = %v", got)
	}
}

func TestNodeActionPayloadAndWait(t *testing.T) {
	f := newFake()
	defer f.close()
	var body map[string]any
	f.on("PUT /api/v1/customers/cust1/universes/u-1/nodes/yb-dev-n1", func(w http.ResponseWriter, r *http.Request) {
		body = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-n"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-n", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "Success", "title": "Stop Node"})
	})
	if err := f.client().NodeActionAndWait(context.Background(), "u-1", "yb-dev-n1", NodeStop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if body["nodeAction"] != "STOP" {
		t.Errorf("body = %v", body)
	}
	if _, err := f.client().NodeAction(context.Background(), "u-1", "", NodeStart); err == nil {
		t.Errorf("empty node name must be rejected")
	}
}

func TestUniverseTopologyAccessors(t *testing.T) {
	u := Universe{UniverseDetails: UniverseDetails{NodeDetailsSet: []NodeDetails{
		{NodeName: "n1", IsTserver: true, State: "Live", CloudInfo: CloudInfo{Region: "us-west1", PrivateIP: "10.150.0.1"}},
		{NodeName: "n2", IsTserver: true, State: "Live", CloudInfo: CloudInfo{Region: "us-east1", PrivateIP: "10.151.0.1"}},
		{NodeName: "n3", IsTserver: true, State: "Stopped", CloudInfo: CloudInfo{Region: "us-east1", PrivateIP: "10.151.0.2"}},
		{NodeName: "m1", IsMaster: true, CloudInfo: CloudInfo{Region: "us-central1"}},
	}}}
	if got := u.Regions(); len(got) != 2 || got[0] != "us-east1" || got[1] != "us-west1" {
		t.Errorf("regions = %v (masters-only regions excluded)", got)
	}
	if got := u.RegionNodes()["us-east1"]; len(got) != 2 || got[0] != "n2" {
		t.Errorf("east nodes = %v", got)
	}
	if got := u.DescribeTopology(); got != "us-east1=2,us-west1=1" {
		t.Errorf("topology = %q", got)
	}
}

// A placed universe goes through /universe_configure and then /universes, and
// what reaches /universes is YBA's own generated placement with the preference
// patched in, never the single-call endpoint, which takes no steerable
// placement.
func TestCreatePlacedUniverseUsesConfigureThenCreate(t *testing.T) {
	f := newFake()
	defer f.close()

	var configureBody, createBody map[string]any
	f.on("POST /api/v1/customers/cust1/universe_configure", func(w http.ResponseWriter, r *http.Request) {
		configureBody = captureBody(t, r)
		writeJSON(w, http.StatusOK, configuredBody())
	})
	f.on("POST /api/v1/customers/cust1/universes", func(w http.ResponseWriter, r *http.Request) {
		createBody = captureBody(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-p", "resourceUUID": "u-p"})
	})

	ref, err := f.client().CreateUniverseFromSpec(context.Background(), multiRegionSpec())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ref.TaskUUID != "task-p" || ref.ResourceUUID != "u-p" {
		t.Fatalf("ref = %+v", ref)
	}
	if slices.Contains(f.requests, "POST /api/v1/customers/cust1/universes/clusters") {
		t.Errorf("the single-call endpoint must not be used for a placed universe: %v", f.requests)
	}

	intent := configureBody["clusters"].([]any)[0].(map[string]any)["userIntent"].(map[string]any)
	if got := intent["regionList"].([]any); len(got) != 3 {
		t.Errorf("configure regionList = %v, want all 3 regions", got)
	}

	cloud := createBody["clusters"].([]any)[0].(map[string]any)["placementInfo"].(map[string]any)["cloudList"].([]any)[0].(map[string]any)
	west := cloud["regionList"].([]any)[0].(map[string]any)
	if got := west["azList"].([]any)[0].(map[string]any)["isAffinitized"]; got != true {
		t.Errorf("created placement: preferred region not affinitized (%v)", got)
	}
	east := cloud["regionList"].([]any)[2].(map[string]any)
	if got := east["azList"].([]any)[0].(map[string]any)["isAffinitized"]; got != false {
		t.Errorf("created placement: non-preferred region still affinitized (%v)", got)
	}
}

// A spec with no placement keeps the single-call endpoint.
func TestCreateUnplacedUniverseKeepsTheSingleCall(t *testing.T) {
	f := newFake()
	defer f.close()
	s := multiRegionSpec()
	s.Placement, s.PreferredRegion = nil, ""
	s.NumNodes = 1

	f.on("POST /api/v1/customers/cust1/universes/clusters", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"taskUUID": "task-s", "resourceUUID": "u-s"})
	})
	if _, err := f.client().CreateUniverseFromSpec(context.Background(), s); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "universe_configure") {
			t.Errorf("an unplaced universe must not take the two-call flow: %v", f.requests)
		}
	}
}

// The shipped keypair default never fails the build: where the shared key is on
// disk it is reused, and where it is not the provider is built without an
// allAccessKeys block. A path the run set explicitly IS required, because that
// was a choice rather than a default.
func TestGCPKeypairDefaultIsBestEffortButAnExplicitPathIsRequired(t *testing.T) {
	resolve := func(string) (string, error) { return writeTempKey(t, `{"type":"service_account"}`), nil }

	spec, err := BuildGCPProviderSpec(fakeConfig{m: map[string]any{}}, resolve)
	if err != nil {
		t.Fatalf("the default keypair must never fail the build: %v", err)
	}
	// Whether the shared key is present differs per host, so assert the contract
	// rather than one host's state: readable means reused, unreadable means
	// omitted, and neither is an error.
	if _, statErr := os.Stat(expandHome(DefaultGCPKeyPairFile)); statErr == nil {
		if spec.KeyPairName != DefaultGCPKeyPairName || spec.KeyPairContent == "" {
			t.Errorf("the shared key is on this host, so it must be reused; got name=%q content=%d bytes",
				spec.KeyPairName, len(spec.KeyPairContent))
		}
	} else if spec.KeyPairName != "" || spec.KeyPairContent != "" {
		t.Errorf("the shared key is absent here, so the block must be omitted; got name=%q", spec.KeyPairName)
	}

	if _, err := BuildGCPProviderSpec(fakeConfig{m: map[string]any{
		"apps.yba.gcp.keypair_file": "/nonexistent/explicitly/chosen.pem",
	}}, resolve); err == nil {
		t.Error("a keypair_file the run set explicitly must be required")
	}
}

func writeTempKey(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
