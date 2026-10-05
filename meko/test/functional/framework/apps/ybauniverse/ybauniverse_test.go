package ybauniverse

import (
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

func provider() *Provider {
	return &Provider{Version: "2026.1.0.0-b1", RunTag: "r1", providerUUID: "p1",
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

func TestNoXClusterOrReadReplicaCapability(t *testing.T) {
	var u universe.Universe = &Universe{}
	if _, err := universe.Capability[universe.XCluster](u); !universe.IsUnsupported(err) {
		t.Error("xcluster")
	}
	if _, err := universe.Capability[universe.Backups](u); err != nil {
		t.Error(err)
	}
}
