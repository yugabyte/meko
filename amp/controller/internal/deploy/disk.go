package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Disk sizing. An endpoint's data volume is backed one of two ways, and the two
// are bounded by different things, so they are sized differently:
//
//   - **PVC** (a storage class is configured): network-backed (PD/EBS). The node
//     the pod lands on is irrelevant, so the size is a straight ratio of the
//     endpoint's CPU.
//   - **emptyDir** (the chart default, no storage class): carved out of the
//     *node's* ephemeral storage, which every pod on that node shares. Sizing it
//     from a fixed ratio would let a few endpoints oversubscribe the node, so it
//     gets the node's own disk-per-vCPU ratio instead: an endpoint asking for N
//     vCPU of the node's CPU gets the matching share of the node's disk.
const (
	// GiB of PVC per vCPU. 5 keeps the default 2-vCPU endpoint at the 10Gi it
	// had before disk became CPU-proportional.
	diskGiBPerVCPU = 5

	// The node lookup is a hard dependency of the emptyDir path: without it the
	// size would have to be guessed, and a guess either oversubscribes the node
	// or silently under-provisions the endpoint. Retry a transient API blip,
	// then fail the create.
	nodeQueryAttempts = 3
	nodeQueryBackoff  = 500 * time.Millisecond
)

// runDirValues mirrors the chart's `runDir` block, which sizes the emptyDir
// backing /data when no storage class is set.
type runDirValues struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
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

// nodeDiskPerVCPU returns the smallest allocatable-ephemeral-storage-per-
// allocatable-cpu ratio (bytes per core) across Ready nodes.
//
// The minimum, not the average: the pod can be scheduled onto any of them, and
// sizing off a roomier node would let it ask for more of a smaller node's disk
// than that node can back.
func parseNodeDiskPerVCPU(body []byte) (float64, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return 0, fmt.Errorf("decode NodeList: %w", err)
	}
	best := 0.0
	for _, n := range list.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		cpuStr, ephStr := n.Status.Allocatable["cpu"], n.Status.Allocatable["ephemeral-storage"]
		if cpuStr == "" || ephStr == "" {
			continue
		}
		cpu, err := parseCPUQuantity(cpuStr)
		if err != nil || cpu <= 0 {
			continue
		}
		eph, err := parseByteQuantity(ephStr)
		if err != nil || eph <= 0 {
			continue
		}
		ratio := float64(eph) / cpu
		if best == 0 || ratio < best {
			best = ratio
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("no Ready node reported both allocatable cpu and ephemeral-storage")
	}
	return best, nil
}

// nodeDiskPerVCPU queries the API for the node ratio, retrying a transient
// failure before giving up.
func (k *KubernetesDeployer) nodeDiskPerVCPU(ctx context.Context) (float64, error) {
	var lastErr error
	for attempt := 1; attempt <= nodeQueryAttempts; attempt++ {
		body, ok := k.k8sAPIGet(ctx, "/api/v1/nodes")
		if ok {
			ratio, err := parseNodeDiskPerVCPU(body)
			if err == nil {
				return ratio, nil
			}
			lastErr = err
		} else {
			lastErr = fmt.Errorf("node list unavailable (not in-cluster, or the API rejected the request)")
		}
		if attempt < nodeQueryAttempts {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(nodeQueryBackoff):
			}
		}
	}
	return 0, fmt.Errorf("after %d attempts: %w", nodeQueryAttempts, lastErr)
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
// the endpoint's CPU. Mutates v; returns an error only for the emptyDir path,
// whose node lookup has no safe fallback.
//
// v.Endpoint.Resources.CPU is the already-defaulted value values() resolved, so
// this never has to re-apply defaultEndpointCPU.
func (k *KubernetesDeployer) applyDiskSizing(ctx context.Context, v *chartValues) error {
	vcpu, err := parseCPUQuantity(v.Endpoint.Resources.CPU)
	if err != nil {
		return fmt.Errorf("disk sizing: %w", err)
	}
	if vcpu <= 0 {
		return fmt.Errorf("disk sizing: endpoint cpu %q is not positive", v.Endpoint.Resources.CPU)
	}

	if v.Persistence != nil {
		// PVC: network-backed, so a flat ratio. --deploy-disk-size acts as a
		// floor so an existing deployment never gets a smaller volume than it
		// asked for.
		want := float64(diskGiBPerVCPU) * vcpu * float64(1<<30)
		if floor, err := parseByteQuantity(k.cfg.DiskSize); err == nil && float64(floor) > want {
			want = float64(floor)
		}
		v.Persistence.Size = gibString(want)
		return nil
	}

	// emptyDir: a share of the node's own disk, proportional to the share of the
	// node's CPU this endpoint reserves.
	ratio, err := k.nodeDiskPerVCPU(ctx)
	if err != nil {
		return fmt.Errorf("disk sizing: cannot read node allocatable to size the endpoint's "+
			"emptyDir /data: %w", err)
	}
	v.RunDir = &runDirValues{SizeLimit: gibString(ratio * vcpu)}
	return nil
}
