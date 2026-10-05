package universe

import (
	"strings"
	"testing"
)

func TestDefaultsMakeAValidRF3(t *testing.T) {
	s := Spec{Name: "u1", Clusters: []ClusterSpec{{}}}.WithDefaults()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	c := s.Primary()
	if c.RF != 3 || c.Tservers() != 3 || c.MasterCount() != 3 || c.Node.MemoryGiB != DefaultMemory {
		t.Errorf("defaults = %+v", c)
	}
}

func TestEveryReplicationFactorAndNodeCountIsExpressible(t *testing.T) {
	for _, rf := range []int{1, 3, 5, 7, 9} {
		for _, nodes := range []int{rf, rf + 2, 12} {
			s := Spec{Name: "u", Clusters: []ClusterSpec{{RF: rf, Placement: map[string]int{"a": nodes}}}}.WithDefaults()
			if err := s.Validate(); err != nil {
				t.Errorf("RF%d on %d nodes: %v", rf, nodes, err)
			}
		}
	}
}

func TestMultiZoneMastersAndReadReplica(t *testing.T) {
	s := Spec{Name: "geo", Clusters: []ClusterSpec{
		{RF: 3, Placement: map[string]int{"gcp.r.a": 2, "gcp.r.b": 2, "gcp.r.c": 2}, Masters: MasterSpec{Placement: map[string]int{"gcp.r.a": 1, "gcp.r.b": 1, "gcp.r.c": 1}}, Node: NodeSpec{CPU: 4, MemoryGiB: 16, DiskGiB: 100, StorageClass: "premium-rwo"}},
		{Type: ReadReplica, RF: 1, Placement: map[string]int{"gcp.r.d": 1}},
	}}.WithDefaults()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := s.Primary().MasterCount(); got != 3 {
		t.Errorf("masters = %d", got)
	}
	if len(s.ReadReplicas()) != 1 || s.ReadReplicas()[0].MasterCount() != 0 {
		t.Errorf("read replicas = %+v", s.ReadReplicas())
	}
	if ZoneName("gcp.r.a") != "a" || ZoneName("b") != "b" {
		t.Error("ZoneName")
	}
}

func TestValidateRefusesWhatNoProviderCanBuild(t *testing.T) {
	rr := ClusterSpec{Type: ReadReplica, RF: 1, Placement: map[string]int{"z": 1}}
	pr := func(rf, nodes int) ClusterSpec {
		return ClusterSpec{Type: Primary, RF: rf, Placement: map[string]int{"z": nodes}}
	}
	cases := map[string]struct {
		spec Spec
		want string
	}{
		"bad name":                {Spec{Name: "Bad_Name", Clusters: []ClusterSpec{pr(1, 1)}}, "universe name"},
		"no clusters":             {Spec{Name: "u"}, "primary cluster"},
		"RF above nodes":          {Spec{Name: "u", Clusters: []ClusterSpec{pr(5, 3)}}, "needs at least 5"},
		"read replica alone":      {Spec{Name: "u", Clusters: []ClusterSpec{rr}}, "needs a primary"},
		"two primaries":           {Spec{Name: "u", Clusters: []ClusterSpec{pr(1, 1), pr(1, 1)}}, "primary must be the first"},
		"RF zero":                 {Spec{Name: "u", Clusters: []ClusterSpec{{Type: Primary, Placement: map[string]int{"z": 1}}}}, "replication factor 0"},
		"no tservers":             {Spec{Name: "u", Clusters: []ClusterSpec{{Type: Primary, RF: 1}}}, "no tservers"},
		"too few masters":         {Spec{Name: "u", Clusters: []ClusterSpec{{Type: Primary, RF: 3, Placement: map[string]int{"z": 3}, Masters: MasterSpec{Count: 1}}}}, "cannot hold"},
		"masters beyond nodes":    {Spec{Name: "u", Clusters: []ClusterSpec{{Type: Primary, RF: 1, Placement: map[string]int{"z": 1}, Masters: MasterSpec{Count: 3}}}}, "masters on 1"},
		"master zone too small":   {Spec{Name: "u", Clusters: []ClusterSpec{{Type: Primary, RF: 1, Placement: map[string]int{"a": 1, "b": 1}, Masters: MasterSpec{Placement: map[string]int{"b": 2}}}}}, "in zone"},
		"replica with masters":    {Spec{Name: "u", Clusters: []ClusterSpec{pr(1, 1), {Type: ReadReplica, RF: 1, Placement: map[string]int{"z": 1}, Masters: MasterSpec{Count: 1}}}}, "no masters"},
		"password without auth":   {Spec{Name: "u", YSQLPassword: "x", Clusters: []ClusterSpec{pr(1, 1)}}, "without YSQLAuth"},
		"RF past the supported 9": {Spec{Name: "u", Clusters: []ClusterSpec{pr(11, 11)}}, "outside 1 to 9"},
	}
	for name, tc := range cases {
		err := tc.spec.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestEvenReplicationFactorIsYugabytesToRefuse(t *testing.T) {
	if err := Single("u", 2, 2).WithDefaults().Validate(); err != nil {
		t.Errorf("RF2 is YugabyteDB's rule to enforce: %v", err)
	}
}

type fakeCfg map[string]any

func (f fakeCfg) GetString(k, d string) string {
	if v, ok := f[k].(string); ok {
		return v
	}
	return d
}
func (f fakeCfg) GetInt(k string, d int) int {
	if v, ok := f[k].(int); ok {
		return v
	}
	return d
}
func (f fakeCfg) GetFloat(k string, d float64) float64 {
	if v, ok := f[k].(float64); ok {
		return v
	}
	return d
}
func (f fakeCfg) GetMap(k string, d map[string]any) map[string]any {
	if v, ok := f[k].(map[string]any); ok {
		return v
	}
	return d
}
func (f fakeCfg) GetStringSlice(k string, d []string) []string {
	if v, ok := f[k].([]string); ok {
		return v
	}
	return d
}

func TestFromConfigSpreadsNodesOverZones(t *testing.T) {
	s := FromConfig(fakeCfg{"db.rf": 5, "db.nodes": 7, "db.zones": []string{"a", "b", "c"}, "db.provider": "yba", "db.cpu": 2.0,
		"db.tserver_gflags": map[string]any{"x": 1}}, "db", "stackdb").WithDefaults()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	c := s.Primary()
	if s.Provider != YBA || c.RF != 5 || c.Tservers() != 7 || c.Placement["a"] != 3 || c.Placement["b"] != 2 || c.Node.CPU != 2 || s.TServerGflags["x"] != "1" {
		t.Errorf("spec = %+v", s)
	}
	d := FromConfig(fakeCfg{}, "db", "d")
	if d.Provider != Yugabyted || d.Primary().RF != 3 || d.Primary().Tservers() != 3 {
		t.Errorf("defaults = %+v", d)
	}
}
