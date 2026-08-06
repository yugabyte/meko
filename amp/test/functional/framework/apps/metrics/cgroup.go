package metrics

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// layout is the cgroup hierarchy a container sits in: v2 (unified) or v1 (legacy
// per-controller trees). It is learned from the probe's own output, so detection
// costs no extra round trip.
type layout int

const (
	layoutUnknown layout = iota
	layoutV2
	layoutV1
)

// cgroupSep separates the CPU and memory halves of the probe output.
const cgroupSep = "@@"

// cpuSample is the previous CPU counter reading (seconds of CPU time consumed).
type cpuSample struct {
	sec float64
	at  time.Time
}

// cgroupCmd reads CPU and memory in ONE exec. With the layout not yet known it
// self-describes: the shell picks the hierarchy and labels which one it read.
func cgroupCmd(l layout) string {
	switch l {
	case layoutV2:
		return "echo V2; cat /sys/fs/cgroup/cpu.stat; echo " + cgroupSep + "; cat /sys/fs/cgroup/memory.current"
	case layoutV1:
		return "echo V1; cat /sys/fs/cgroup/cpuacct/cpuacct.usage; echo " + cgroupSep +
			"; cat /sys/fs/cgroup/memory/memory.usage_in_bytes"
	default:
		return "if [ -f /sys/fs/cgroup/cpu.stat ]; then " + cgroupCmd(layoutV2) +
			"; else " + cgroupCmd(layoutV1) + "; fi"
	}
}

// cgroupReading is the parsed probe output: CPU as a counter (seconds of CPU
// time consumed) and memory as a gauge (bytes).
type cgroupReading struct {
	layout   layout
	cpuSec   float64
	memBytes float64
	haveCPU  bool
	haveMem  bool
}

// parseCgroup reads the labelled probe output. v2 reports cpu.stat's usage_usec
// (microseconds); v1 reports cpuacct.usage (nanoseconds) as a bare number.
func parseCgroup(out string) cgroupReading {
	var r cgroupReading
	lines := strings.Split(out, "\n")
	inMem := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "V2":
			r.layout = layoutV2
			continue
		case line == "V1":
			r.layout = layoutV1
			continue
		case line == cgroupSep:
			inMem = true
			continue
		}
		if inMem {
			if v, err := strconv.ParseFloat(line, 64); err == nil {
				r.memBytes, r.haveMem = v, true
			}
			continue
		}
		switch r.layout {
		case layoutV2:
			usec, ok := strings.CutPrefix(line, "usage_usec ")
			if !ok {
				continue
			}
			if v, err := strconv.ParseFloat(strings.TrimSpace(usec), 64); err == nil {
				r.cpuSec, r.haveCPU = v/1e6, true
			}
		case layoutV1:
			if v, err := strconv.ParseFloat(line, 64); err == nil {
				r.cpuSec, r.haveCPU = v/1e9, true
			}
		}
	}
	return r
}

// sampleCgroup probes one pod's CPU and memory. Memory is a gauge and reports
// every tick; CPU is a counter, so the first tick only seeds the baseline and
// every later tick reports cores used = Δcpu-seconds / Δwall-clock.
func sampleCgroup(ctx context.Context, ex runner, st *podState, now time.Time, logf func(string, ...any)) []reading {
	res, err := ex.Run(ctx, cgroupCmd(st.cgroup))
	if err != nil || res.ExitCode != 0 {
		logf("metrics: cgroup probe failed (exit %d): %v", res.ExitCode, err)
		return nil
	}
	cr := parseCgroup(res.Stdout)
	if cr.layout != layoutUnknown {
		st.cgroup = cr.layout
	}

	var out []reading
	if cr.haveMem {
		out = append(out, reading{name: "Memory", unit: "MiB", round: 1, value: cr.memBytes / (1 << 20), at: now})
	}
	if cr.haveCPU {
		if !st.cpu.at.IsZero() {
			dt := now.Sub(st.cpu.at).Seconds()
			delta := cr.cpuSec - st.cpu.sec
			if dt > 0 && delta >= 0 {
				out = append(out, reading{name: "CPU", unit: "cores", round: 2, value: delta / dt, at: now})
			}
		}
		st.cpu = cpuSample{sec: cr.cpuSec, at: now}
	}
	return out
}
