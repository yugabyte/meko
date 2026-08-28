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
	// GiB of data volume per vCPU. This is the policy, not a fallback: an
	// endpoint's /data is a function of its CPU alone, at 20 GiB per vCPU, so a
	// 32-vCPU perf endpoint gets 640 GiB (#602).
	//
	// Deliberately uncapped. A pool that cannot honour the ratio is a pool that
	// was provisioned too small, and the right failure for that is the scheduler
	// leaving the pod Pending — not silently handing the endpoint a volume that
	// its CPU allocation will overrun. --deploy-disk-gib-per-vcpu overrides the
	// ratio for a pool built to a deliberately different disk-per-core shape,
	// and --deploy-disk-size-override abandons the ratio altogether for a test
	// cluster that just needs endpoints to fit (see diskBytesFor).
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
//
// --deploy-disk-size-override replaces that calculation outright: every endpoint
// gets exactly that size, whatever its CPU. It is for test clusters, whose nodes
// cannot back the ratio — a GKE node on the default 100GB boot disk has ~43.8
// GiB allocatable, so a 2-vCPU endpoint's 40 GiB request packs one endpoint per
// node, and under a cluster-wide autoprovisioning quota the next one stays
// Pending indefinitely (#602). Not for prod: a flat size is a promise that
// nothing will fill the disk, which holds for a test's scratch data and not for
// a real workload.
func (k *KubernetesDeployer) diskBytesFor(vcpu float64) float64 {
	// Config validation rejects an unparseable override at startup; falling back
	// to the ratio keeps a directly-constructed deployer from sizing to 0.
	if k.cfg.DiskSizeOverride != "" {
		if flat, err := ParseByteQuantity(k.cfg.DiskSizeOverride); err == nil && flat > 0 {
			return float64(flat)
		}
	}
	ratio := k.cfg.DiskGiBPerVCPU
	if ratio <= 0 {
		ratio = defaultDiskGiBPerVCPU
	}
	want := float64(ratio) * vcpu * float64(1<<30)
	if floor, err := ParseByteQuantity(k.cfg.DiskSize); err == nil && float64(floor) > want {
		want = float64(floor)
	}
	return want
}

// ParseCPUQuantity parses a Kubernetes CPU quantity ("2", "1500m", "0.5") into
// cores.
//
// Exported for the API layer, which needs the same reading of a quantity to
// check an endpoint's memory-to-CPU shape before it accepts the request.
func ParseCPUQuantity(s string) (float64, error) {
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

// ParseByteQuantity parses a Kubernetes storage/memory quantity ("10Gi",
// "98831908Ki", "2G", "1073741824") into bytes.
//
// Exported alongside [ParseCPUQuantity], and for the same reason.
func ParseByteQuantity(s string) (int64, error) {
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
	vcpu, err := ParseCPUQuantity(v.Endpoint.Resources.CPU)
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
