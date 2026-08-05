package deploy

import (
	"context"
	"testing"
)

func TestParseCPUQuantity(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "2", want: 2},
		{in: "1500m", want: 1.5},
		{in: "500m", want: 0.5},
		{in: "0.5", want: 0.5},
		{in: " 4 ", want: 4},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	} {
		got, err := parseCPUQuantity(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCPUQuantity(%q): want error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCPUQuantity(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseCPUQuantity(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseByteQuantity(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "10Gi", want: 10 << 30},
		{in: "512Mi", want: 512 << 20},
		{in: "98831908Ki", want: 98831908 << 10},
		{in: "2G", want: 2_000_000_000},
		{in: "1073741824", want: 1 << 30},
		{in: "", wantErr: true},
		{in: "nope", wantErr: true},
	} {
		got, err := parseByteQuantity(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseByteQuantity(%q): want error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseByteQuantity(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseByteQuantity(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The emptyDir share must come off the *smallest* node ratio: the pod can land
// on any Ready node, and sizing off a roomier one would over-promise on a
// smaller node's disk. NotReady nodes are ignored entirely.
func TestParseNodeDiskPerVCPU(t *testing.T) {
	body := []byte(`{"items":[
	  {"metadata":{"name":"big"},"status":{
	     "allocatable":{"cpu":"8","ephemeral-storage":"80Gi"},
	     "conditions":[{"type":"Ready","status":"True"}]}},
	  {"metadata":{"name":"small"},"status":{
	     "allocatable":{"cpu":"8","ephemeral-storage":"40Gi"},
	     "conditions":[{"type":"Ready","status":"True"}]}},
	  {"metadata":{"name":"tiny-but-notready"},"status":{
	     "allocatable":{"cpu":"8","ephemeral-storage":"8Gi"},
	     "conditions":[{"type":"Ready","status":"False"}]}}
	]}`)
	got, err := parseNodeDiskPerVCPU(body)
	if err != nil {
		t.Fatalf("parseNodeDiskPerVCPU: %v", err)
	}
	want := float64(40<<30) / 8 // the "small" Ready node: 5 GiB per core
	if got != want {
		t.Fatalf("ratio = %v, want %v (min across Ready nodes)", got, want)
	}
	if s := gibString(got * 4); s != "20Gi" {
		t.Fatalf("4 vCPU at 5GiB/core = %s, want 20Gi", s)
	}
}

func TestParseNodeDiskPerVCPUErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no nodes":            `{"items":[]}`,
		"none ready":          `{"items":[{"status":{"allocatable":{"cpu":"8","ephemeral-storage":"80Gi"},"conditions":[{"type":"Ready","status":"False"}]}}]}`,
		"no ephemeral field":  `{"items":[{"status":{"allocatable":{"cpu":"8"},"conditions":[{"type":"Ready","status":"True"}]}}]}`,
		"unparseable numbers": `{"items":[{"status":{"allocatable":{"cpu":"x","ephemeral-storage":"y"},"conditions":[{"type":"Ready","status":"True"}]}}]}`,
	} {
		if _, err := parseNodeDiskPerVCPU([]byte(body)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestGibStringRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{in: 10 << 30, want: "10Gi"},
		{in: 10.5 * (1 << 30), want: "11Gi"}, // partial GiB rounds up
		{in: 1, want: "1Gi"},                 // never below 1Gi
		{in: 0, want: "1Gi"},
	} {
		if got := gibString(tc.in); got != tc.want {
			t.Errorf("gibString(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// PVC sizing is CPU-proportional and needs no cluster access; --deploy-disk-size
// only acts as a floor.
func TestApplyDiskSizingPVC(t *testing.T) {
	for _, tc := range []struct {
		name, cpu, floor, want string
	}{
		{name: "default 2 vCPU keeps 10Gi", cpu: "2", floor: "10Gi", want: "10Gi"},
		{name: "4 vCPU doubles", cpu: "4", floor: "10Gi", want: "20Gi"},
		{name: "8 vCPU", cpu: "8", floor: "10Gi", want: "40Gi"},
		{name: "floor wins for a small endpoint", cpu: "1", floor: "10Gi", want: "10Gi"},
		{name: "no floor configured", cpu: "4", floor: "", want: "20Gi"},
		{name: "fractional cpu rounds up", cpu: "1500m", floor: "", want: "8Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: tc.floor, StorageClass: "std"}}
			v := chartValues{
				Endpoint:    endpointValues{Resources: resourceValues{CPU: tc.cpu}},
				Persistence: &persistenceValues{StorageClass: "std"},
			}
			if err := k.applyDiskSizing(context.Background(), &v); err != nil {
				t.Fatalf("applyDiskSizing: %v", err)
			}
			if v.Persistence.Size != tc.want {
				t.Errorf("PVC size = %s, want %s", v.Persistence.Size, tc.want)
			}
			if v.RunDir != nil {
				t.Errorf("PVC path must not set runDir, got %+v", v.RunDir)
			}
		})
	}
}

// The emptyDir path has no safe fallback: out of cluster the node lookup fails,
// and the create must fail rather than guess a size.
func TestApplyDiskSizingEmptyDirFailsWithoutNodes(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: "10Gi"}}
	v := chartValues{Endpoint: endpointValues{Resources: resourceValues{CPU: "2"}}}
	err := k.applyDiskSizing(context.Background(), &v)
	if err == nil {
		t.Fatal("want an error when node allocatable cannot be read, got nil")
	}
	if v.RunDir != nil {
		t.Errorf("runDir must stay unset on failure, got %+v", v.RunDir)
	}
}

func TestApplyDiskSizingRejectsBadCPU(t *testing.T) {
	k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: "10Gi"}}
	for _, cpu := range []string{"", "abc", "0", "-1"} {
		v := chartValues{Endpoint: endpointValues{Resources: resourceValues{CPU: cpu}}}
		if err := k.applyDiskSizing(context.Background(), &v); err == nil {
			t.Errorf("cpu %q: want error, got nil", cpu)
		}
	}
}
