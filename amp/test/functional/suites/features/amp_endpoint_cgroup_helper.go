package features

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// cgroupProbe prints the container's cpu and memory limits, cgroup v2 first.
const cgroupProbe = `cat /sys/fs/cgroup/cpu.max 2>/dev/null || echo "$(cat /sys/fs/cgroup/cpu/cpu.cfs_quota_us) $(cat /sys/fs/cgroup/cpu/cpu.cfs_period_us)"; cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes`

// podCgroupLimits reads the live cpu (millicores, -1 unlimited) and memory
// (bytes, -1 unlimited) limits of the pod's default container.
func podCgroupLimits(ctx context.Context, kube *k8s.Kubectl, pod string) (int64, int64, error) {
	res, err := kube.ForPod(pod, "").Run(ctx, cgroupProbe)
	if err != nil {
		return 0, 0, fmt.Errorf("read cgroup limits in pod %s: %w", pod, err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if res.ExitCode != 0 || len(lines) != 2 {
		return 0, 0, fmt.Errorf("cgroup probe in pod %s exited %d with %q", pod, res.ExitCode, res.Stdout)
	}
	cpu, err := parseCgroupCPU(lines[0])
	if err != nil {
		return 0, 0, fmt.Errorf("pod %s: %w", pod, err)
	}
	mem, err := parseCgroupMemory(lines[1])
	if err != nil {
		return 0, 0, fmt.Errorf("pod %s: %w", pod, err)
	}
	return cpu, mem, nil
}

func parseCgroupCPU(line string) (int64, error) {
	f := strings.Fields(line)
	if len(f) != 2 {
		return 0, fmt.Errorf("cpu.max line %q, want \"<quota> <period>\"", line)
	}
	if f[0] == "max" || f[0] == "-1" {
		return -1, nil
	}
	quota, err1 := strconv.ParseInt(f[0], 10, 64)
	period, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || period <= 0 {
		return 0, fmt.Errorf("cpu.max line %q is not numeric", line)
	}
	return quota * 1000 / period, nil
}

func parseCgroupMemory(line string) (int64, error) {
	s := strings.TrimSpace(line)
	if s == "max" {
		return -1, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory.max line %q is not numeric", line)
	}
	if n >= 1<<62 {
		return -1, nil
	}
	return n, nil
}

// parseCPUMilli reads a Kubernetes cpu quantity ("2", "1500m") as millicores.
func parseCPUMilli(q string) (int64, error) {
	q = strings.TrimSpace(q)
	if strings.HasSuffix(q, "m") {
		return strconv.ParseInt(strings.TrimSuffix(q, "m"), 10, 64)
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, fmt.Errorf("cpu quantity %q", q)
	}
	return int64(f*1000 + 0.5), nil
}

// parseMemBytes reads a Kubernetes memory quantity ("16Gi", "512Mi", "1G") as bytes.
func parseMemBytes(q string) (int64, error) {
	q = strings.TrimSpace(q)
	units := []struct {
		suffix string
		mult   int64
	}{{"Ei", 1 << 60}, {"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"E", 1e18}, {"P", 1e15}, {"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"k", 1e3}}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(q, u.suffix), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("memory quantity %q", q)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory quantity %q", q)
	}
	return n, nil
}

// assertPodCgroup holds the running container to the reservation the control
// plane reports, which is the assertion "the API answer, not the pod" lacked.
func assertPodCgroup(ctx context.Context, kube *k8s.Kubectl, pod, wantCPU, wantMemory string) error {
	cpu, mem, err := podCgroupLimits(ctx, kube, pod)
	if err != nil {
		return err
	}
	wantMilli, err := parseCPUMilli(wantCPU)
	if err != nil {
		return err
	}
	wantBytes, err := parseMemBytes(wantMemory)
	if err != nil {
		return err
	}
	if cpu != wantMilli || mem != wantBytes {
		return fmt.Errorf("pod %s runs with cgroup limits cpu=%dm memory=%d bytes while the control plane reports cpu=%s memory=%s: the persisted reservation and the live container disagree (meko#905)", pod, cpu, mem, wantCPU, wantMemory)
	}
	return nil
}
