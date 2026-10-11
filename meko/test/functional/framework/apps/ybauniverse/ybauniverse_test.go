package ybauniverse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

func provider() *Provider {
	return &Provider{Version: "2026.1.0.0-b1", RunTag: "r1", reg: &registration{uuid: "p1"},
		K8sProvider: yba.K8sProviderSpec{Regions: []yba.K8sRegionSpec{{Code: "us-west1"}}}}
}

func TestBuildSpecMapsTheNodeShapeAndFlags(t *testing.T) {
	s := universe.Spec{Name: "u1", Version: "2025.2.1.0-b1", MasterGflags: map[string]string{"a": "1"},
		Clusters: []universe.ClusterSpec{{RF: 5, Placement: map[string]int{"z": 7}, Node: universe.NodeSpec{CPU: 3.5, MemoryGiB: 12, DiskGiB: 80, StorageClass: "premium-rwo"}, TServerGflags: map[string]string{"b": "2"}}}}.WithDefaults()
	us, err := provider().BuildSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	if us.NumNodes != 7 || us.ReplicationFactor != 5 || us.TServerCPUCores != 4 || us.TServerMemGiB != 12 || us.VolumeSizeGB != 80 ||
		us.K8sStorageClass != "premium-rwo" || us.YBSoftwareVersion != "2025.2.1.0-b1" || us.MasterGFlags["a"] != "1" || us.TServerGFlags["b"] != "2" || us.Region != "us-west1" {
		t.Errorf("spec = %+v", us)
	}
	if len(us.Placement) != 0 {
		t.Error("one zone needs no placement")
	}
}

func TestMultiZonePlacementGoesToYBA(t *testing.T) {
	s := universe.Spec{Name: "u", Clusters: []universe.ClusterSpec{{RF: 3, Placement: map[string]int{"a": 1, "b": 2}}}}.WithDefaults()
	us, err := provider().BuildSpec(s)
	if err != nil || len(us.Placement) != 1 || len(us.Placement[0].Zones) != 2 {
		t.Fatalf("%+v %v", us.Placement, err)
	}
}

func TestWhatYBACannotBeToldIsUnsupported(t *testing.T) {
	two := universe.Spec{Name: "u", Clusters: []universe.ClusterSpec{{RF: 1}, {Type: universe.ReadReplica, RF: 1}}}.WithDefaults()
	if _, err := provider().BuildSpec(two); !universe.IsUnsupported(err) {
		t.Errorf("read replica at create: %v", err)
	}
	img := universe.Spec{Name: "u", Image: "x/y:1", Clusters: []universe.ClusterSpec{{RF: 1}}}.WithDefaults()
	_, err := provider().BuildSpec(img)
	if !universe.IsUnsupported(err) || !strings.Contains(err.Error(), "image") {
		t.Errorf("image: %v", err)
	}
}

func TestYBAHasReplicationCapabilities(t *testing.T) {
	var u universe.Universe = &Universe{}
	if _, err := universe.Capability[universe.XCluster](u); err != nil {
		t.Error(err)
	}
	if _, err := universe.Capability[universe.ReadReplicas](u); err != nil {
		t.Error(err)
	}
	if _, err := universe.Capability[universe.Backups](u); err != nil {
		t.Error(err)
	}
}

func TestReadReplicaCopiesThePrimaryWithoutOverrides(t *testing.T) {
	const universeJSON = `{"universeUUID":"u1","universeDetails":{"updateSucceeded":true,"clusters":[{"uuid":"c1","clusterType":"PRIMARY",
		"userIntent":{"numNodes":3,"replicationFactor":3,"provider":"p1","universeOverrides":"master: {}"}}]}}`
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/clusters/read_only"):
			_ = json.NewDecoder(r.Body).Decode(&sent)
			fmt.Fprint(w, `{"taskUUID":"t1","resourceUUID":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/tasks/t1"):
			fmt.Fprint(w, `{"status":"Success","percent":100}`)
		case strings.HasSuffix(r.URL.Path, "/universes/u1"):
			fmt.Fprint(w, universeJSON)
		case strings.HasSuffix(r.URL.Path, "/universes"):
			fmt.Fprint(w, "["+universeJSON+"]")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u := &Universe{p: &Provider{Client: yba.New(srv.URL, "c", "tok")}, id: "u1", spec: universe.Spec{Name: "rr"}}
	if err := u.AddReadReplica(context.Background(), universe.ClusterSpec{RF: 1, Placement: map[string]int{"z": 1}}); err != nil {
		t.Fatal(err)
	}
	if sent["universeUUID"] != "u1" {
		t.Errorf("universeUUID = %v, YBA needs it to find the primary", sent["universeUUID"])
	}
	cl := sent["clusters"].([]any)
	if len(cl) != 1 {
		t.Fatalf("clusters = %v, YBA takes only the new one", cl)
	}
	c := cl[0].(map[string]any)
	ui := c["userIntent"].(map[string]any)
	if c["clusterType"] != "ASYNC" || ui["numNodes"] != 1.0 || ui["replicationFactor"] != 1.0 || ui["provider"] != "p1" {
		t.Errorf("cluster = %v", c)
	}
	if _, ok := ui["universeOverrides"]; ok {
		t.Error("YBA refuses a read-only cluster with overrides")
	}
}
