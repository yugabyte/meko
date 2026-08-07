package deploy

import (
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
		ratio                  int
	}{
		{name: "default 2 vCPU at 20 GiB/vCPU", cpu: "2", floor: "10Gi", want: "40Gi"},
		{name: "4 vCPU doubles", cpu: "4", floor: "10Gi", want: "80Gi"},
		{name: "floor wins for a small endpoint", cpu: "1", floor: "30Gi", want: "30Gi"},
		{name: "no floor configured", cpu: "4", floor: "", want: "80Gi"},
		{name: "fractional cpu rounds up", cpu: "1500m", floor: "", want: "30Gi"},
		{name: "installer ratio wins over the default", cpu: "2", floor: "", ratio: 100, want: "200Gi"},
		// #602: the ratio holds all the way up. A 32-vCPU perf endpoint asks for
		// the full 640Gi; nothing clamps it to a "reasonable" ceiling, because a
		// pool that cannot back the ratio should fail to schedule rather than
		// quietly under-provision the endpoint.
		{name: "32 vCPU perf endpoint is uncapped", cpu: "32", floor: "10Gi", want: "640Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: tc.floor, StorageClass: "std", DiskGiBPerVCPU: tc.ratio}}
			v := chartValues{
				Endpoint:    endpointValues{Resources: resourceValues{CPU: tc.cpu}},
				Persistence: &persistenceValues{StorageClass: "std"},
			}
			if err := k.applyDiskSizing(&v); err != nil {
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

// The emptyDir path takes the SAME flat ratio as the PVC path, and needs no
// cluster access at all — that dependency is what #584 was about. It also
// declares the size as an ephemeral-storage request, which is what makes the
// scheduler account the volume against the node.
func TestApplyDiskSizingEmptyDir(t *testing.T) {
	// Deliberately out of cluster: sizing must not consult the API server.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	for _, tc := range []struct {
		name, cpu, floor, want string
		ratio                  int
	}{
		{name: "default 2 vCPU at 20 GiB/vCPU", cpu: "2", floor: "10Gi", want: "40Gi"},
		{name: "4 vCPU doubles", cpu: "4", floor: "10Gi", want: "80Gi"},
		{name: "floor wins for a small endpoint", cpu: "1", floor: "30Gi", want: "30Gi"},
		{name: "no floor configured", cpu: "1", floor: "", want: "20Gi"},
		{name: "installer ratio wins over the default", cpu: "1", floor: "", ratio: 40, want: "40Gi"},
		// #602: same uncapped ratio on the emptyDir side, where the 640Gi is also
		// the ephemeral-storage request the scheduler accounts against the node.
		{name: "32 vCPU perf endpoint is uncapped", cpu: "32", floor: "10Gi", want: "640Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: tc.floor, DiskGiBPerVCPU: tc.ratio}}
			v := chartValues{Endpoint: endpointValues{Resources: resourceValues{CPU: tc.cpu}}}
			if err := k.applyDiskSizing(&v); err != nil {
				t.Fatalf("applyDiskSizing: %v", err)
			}
			if v.RunDir == nil || v.RunDir.SizeLimit != tc.want {
				t.Errorf("emptyDir sizeLimit = %+v, want %s", v.RunDir, tc.want)
			}
			if v.Endpoint.Resources.EphemeralStorage != tc.want {
				t.Errorf("ephemeral-storage request = %q, want %s",
					v.Endpoint.Resources.EphemeralStorage, tc.want)
			}
			if v.Persistence != nil {
				t.Errorf("emptyDir path must not set persistence, got %+v", v.Persistence)
			}
		})
	}
}

// A PVC is not ephemeral storage, so claiming it against the node's disk budget
// would reserve the same bytes twice.
func TestApplyDiskSizingPVCRequestsNoEphemeralStorage(t *testing.T) {
	k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: "10Gi", StorageClass: "std"}}
	v := chartValues{
		Endpoint:    endpointValues{Resources: resourceValues{CPU: "4"}},
		Persistence: &persistenceValues{StorageClass: "std"},
	}
	if err := k.applyDiskSizing(&v); err != nil {
		t.Fatalf("applyDiskSizing: %v", err)
	}
	if v.Endpoint.Resources.EphemeralStorage != "" {
		t.Errorf("ephemeral-storage = %q, want empty for a PVC-backed endpoint",
			v.Endpoint.Resources.EphemeralStorage)
	}
}

// Both backings agree: the size is a function of CPU alone.
func TestApplyDiskSizingSameSizeBothBackings(t *testing.T) {
	k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: "10Gi"}}
	empty := chartValues{Endpoint: endpointValues{Resources: resourceValues{CPU: "8"}}}
	pvc := chartValues{
		Endpoint:    endpointValues{Resources: resourceValues{CPU: "8"}},
		Persistence: &persistenceValues{StorageClass: "std"},
	}
	if err := k.applyDiskSizing(&empty); err != nil {
		t.Fatalf("emptyDir: %v", err)
	}
	if err := k.applyDiskSizing(&pvc); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if empty.RunDir.SizeLimit != pvc.Persistence.Size {
		t.Errorf("emptyDir %s != pvc %s", empty.RunDir.SizeLimit, pvc.Persistence.Size)
	}
}

func TestApplyDiskSizingRejectsBadCPU(t *testing.T) {
	k := &KubernetesDeployer{cfg: KubernetesConfig{DiskSize: "10Gi"}}
	for _, cpu := range []string{"", "abc", "0", "-1"} {
		v := chartValues{Endpoint: endpointValues{Resources: resourceValues{CPU: cpu}}}
		if err := k.applyDiskSizing(&v); err == nil {
			t.Errorf("cpu %q: want error, got nil", cpu)
		}
	}
}
