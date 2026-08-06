package deploy

import (
	"fmt"
	"strconv"
	"strings"
)

// Disk sizing. Both backings of an endpoint's data volume get the same
// CPU-proportional size, and the difference between them is who enforces it:
//
//   - **PVC** (a storage class is configured): network-backed (PD/EBS). The
//     volume is its own device, so the size is simply what was asked for.
//   - **emptyDir** (the chart default, no storage class): carved out of the
//     *node's* ephemeral storage, which every pod on that node shares. The size
//     becomes both the volume's sizeLimit and an `ephemeral-storage` request, so
//     the SCHEDULER refuses to place more endpoints on a node than its disk can
//     back. That is what keeps co-tenants from oversubscribing it.
//
// Deliberately no node inspection. Deriving the size from the node's own
// disk-per-vCPU ratio needs a cluster-scoped `nodes` read, which BYOC customers
// will not grant (#584) — and it could only ever be an estimate, because the
// controller knows neither which node the pod will land on nor who else will
// land there later. The scheduler knows both, and `ephemeral-storage` is how it
// is told. A declared request is stronger than a measured guess.
const (
	// GiB of data volume per vCPU when the installer passes no ratio
	// (--deploy-disk-gib-per-vcpu). The right value depends on the node type —
	// a local-NVMe pool has far more disk per core than a boot-disk one — and
	// only the installer knows which nodes it bought, so this is a floor-ish
	// default, not a policy.
	defaultDiskGiBPerVCPU = 20
)

// runDirValues mirrors the chart's `runDir` block, which sizes the emptyDir
// backing /data when no storage class is set.
type runDirValues struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// diskBytesFor returns the data volume size for an endpoint reserving vcpu
// cores: the installer's per-vCPU ratio (or the built-in default), floored by
// --deploy-disk-size so an existing deployment never gets a smaller volume
// than it asked for.
func (k *KubernetesDeployer) diskBytesFor(vcpu float64) float64 {
	ratio := k.cfg.DiskGiBPerVCPU
	if ratio <= 0 {
		ratio = defaultDiskGiBPerVCPU
	}
	want := float64(ratio) * vcpu * float64(1<<30)
	if floor, err := parseByteQuantity(k.cfg.DiskSize); err == nil && float64(floor) > want {
		want = float64(floor)
	}
	return want
}

// parseCPUQuantity parses a Kubernetes CPU quantity ("2", "1500m", "0.5") into
// cores.
func parseCPUQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty cpu quantity")
	}
	if rest, ok := strings.CutSuffix(s, "m"); ok {
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu quantity %q: %w", s, err)
		}
		return v / 1000, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("cpu quantity %q: %w", s, err)
	}
	return v, nil
}

var byteSuffixes = []struct {
	suffix string
	mult   float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
	{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
}

// parseByteQuantity parses a Kubernetes storage/memory quantity ("10Gi",
// "98831908Ki", "2G", "1073741824") into bytes.
func parseByteQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty byte quantity")
	}
	for _, b := range byteSuffixes {
		if rest, ok := strings.CutSuffix(s, b.suffix); ok {
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return 0, fmt.Errorf("byte quantity %q: %w", s, err)
			}
			return int64(v * b.mult), nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("byte quantity %q: %w", s, err)
	}
	return int64(v), nil
}

// gibString rounds bytes up to whole GiB, floored at 1Gi, as a k8s quantity.
func gibString(bytes float64) string {
	const gib = float64(1 << 30)
	g := int64((bytes + gib - 1) / gib)
	if g < 1 {
		g = 1
	}
	return fmt.Sprintf("%dGi", g)
}

// applyDiskSizing fills in the data volume's size on the rendered values, from
// the endpoint's CPU. Mutates v.
//
// v.Endpoint.Resources.CPU is the already-defaulted value values() resolved, so
// this never has to re-apply defaultEndpointCPU.
func (k *KubernetesDeployer) applyDiskSizing(v *chartValues) error {
	vcpu, err := parseCPUQuantity(v.Endpoint.Resources.CPU)
	if err != nil {
		return fmt.Errorf("disk sizing: %w", err)
	}
	if vcpu <= 0 {
		return fmt.Errorf("disk sizing: endpoint cpu %q is not positive", v.Endpoint.Resources.CPU)
	}
	size := gibString(k.diskBytesFor(vcpu))

	if v.Persistence != nil {
		// A PVC is its own device: the size is the claim, and nothing on the node
		// is being shared.
		v.Persistence.Size = size
		return nil
	}

	// emptyDir: the sizeLimit is only an eviction ceiling — it reserves nothing —
	// so the same figure is declared as an ephemeral-storage request/limit, which
	// is what the scheduler accounts against the node's capacity.
	v.RunDir = &runDirValues{SizeLimit: size}
	v.Endpoint.Resources.EphemeralStorage = size
	return nil
}
