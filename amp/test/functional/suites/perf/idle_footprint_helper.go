package perf

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const procStatCmd = "getconf PAGESIZE; cat /proc/[0-9]*/stat 2>/dev/null"

const (
	// USER_HZ, 100 on every platform amp ships on, and /proc offers no way to ask.
	clockTicks = 100.0
	// Only the fallback: field 22 is in pages, and a 64 KiB-page arm64 node would report every RSS 16x low.
	defaultPageBytes = 4096.0

	otherProcGroup = "other"
)

// CPUMilli and RSSMiB are medians over the hold's intervals and sweeps, so one
// burst (a compaction, a GC pass) cannot set the number; CPURates and RSSRuns
// are those samples. CPURates[i] is interval i for every group.
type procUsage struct {
	Name     string
	CPUMilli float64
	RSSMiB   float64
	Procs    int
	CPURates []float64
	RSSRuns  []float64
}

type procSample struct {
	At        time.Time
	PageBytes float64
	Ticks     map[string]float64
	Pages     map[string]float64
	Counts    map[string]int
	// PidTicks and PidGroup key utime+stime by pid, so an interval's CPU is
	// the survivors' deltas: a group total drops by a whole lifetime when one
	// of its processes exits.
	PidTicks map[string]float64
	PidGroup map[string]string
}

// comm is parenthesised and may contain spaces, so fields are counted from after the ")": utime and stime 12th and 13th, rss 22nd.
func parseProcStat(out string, at time.Time) procSample {
	s := procSample{At: at, PageBytes: defaultPageBytes, Ticks: map[string]float64{}, Pages: map[string]float64{}, Counts: map[string]int{},
		PidTicks: map[string]float64{}, PidGroup: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(line), 64); err == nil && v > 0 {
			s.PageBytes = v
			continue
		}
		lo := strings.Index(line, "(")
		hi := strings.LastIndex(line, ")")
		if lo < 0 || hi < lo {
			continue
		}
		comm := line[lo+1 : hi]
		fields := strings.Fields(line[hi+1:])
		if len(fields) < 22 {
			continue
		}
		utime, err1 := strconv.ParseFloat(fields[11], 64)
		stime, err2 := strconv.ParseFloat(fields[12], 64)
		rss, err3 := strconv.ParseFloat(fields[21], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		g := procGroup(comm)
		if pid := strings.TrimSpace(line[:lo]); pid != "" {
			s.PidTicks[pid] = utime + stime
			s.PidGroup[pid] = g
		}
		s.Ticks[g] += utime + stime
		s.Pages[g] += rss
		s.Counts[g]++
	}
	return s
}

func procGroup(comm string) string {
	low := strings.ToLower(comm)
	switch {
	case strings.Contains(low, "pageserver"):
		return "pageserver"
	case strings.Contains(low, "safekeeper"):
		return "safekeeper"
	case strings.Contains(low, "compute-ctl"), strings.Contains(low, "compute_ctl"):
		return "compute-ctl"
	// /proc truncates comm at 15 chars, so the supervisor arrives as "endpoint-superv".
	case strings.Contains(low, "superv"):
		return "supervisor"
	case strings.HasPrefix(low, "postgres"), strings.HasPrefix(low, "postmaster"):
		return "postgres"
	default:
		return otherProcGroup
	}
}

// intervalTicks is each group's CPU ticks between two sweeps: every surviving
// pid's delta, plus the whole count of a pid born in the interval. A pid that
// exited is lost for its last interval, which /proc cannot recover.
func intervalTicks(prev, cur procSample) map[string]float64 {
	out := map[string]float64{}
	for g := range cur.Ticks {
		out[g] = 0
	}
	for pid, ticks := range cur.PidTicks {
		d := ticks
		if before, ok := prev.PidTicks[pid]; ok {
			d = ticks - before
		}
		out[cur.PidGroup[pid]] += max(d, 0)
	}
	return out
}

func sampleIdleProcesses(ctx context.Context, pod *k8s.Kubectl, hold, interval time.Duration, logf func(string, ...any)) ([]procUsage, error) {
	if interval <= 0 {
		interval = DefaultIdleInterval
	}
	sweep := func() (procSample, bool) {
		res, err := pod.Run(ctx, procStatCmd)
		if err != nil || res.ExitCode != 0 {
			logf("idle_footprint: /proc sweep failed (exit %d): %v", res.ExitCode, err)
			return procSample{}, false
		}
		return parseProcStat(res.Stdout, time.Now()), true
	}

	prev, ok := sweep()
	if !ok {
		return nil, fmt.Errorf("the first /proc sweep failed, so nothing was measured")
	}
	if len(prev.Counts) == 0 {
		return nil, fmt.Errorf("the first /proc sweep parsed no process, so nothing was measured")
	}
	cpuRates := map[string][]float64{}
	rss := map[string][]float64{}
	procs := map[string]int{}
	intervals := 0
	addGauges := func(s procSample) {
		for g, pages := range s.Pages {
			rss[g] = append(rss[g], pages*s.PageBytes/(1<<20))
			procs[g] = max(procs[g], s.Counts[g])
		}
	}
	addGauges(prev)

	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		wait := min(interval, time.Until(deadline))
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
		cur, ok := sweep()
		if !ok {
			continue
		}
		dt := cur.At.Sub(prev.At).Seconds()
		if dt > 0 {
			for g, ticks := range intervalTicks(prev, cur) {
				// A group first seen now idled at 0 in every earlier interval.
				for len(cpuRates[g]) < intervals {
					cpuRates[g] = append(cpuRates[g], 0)
				}
				cpuRates[g] = append(cpuRates[g], ticks/clockTicks/dt*1000)
			}
			intervals++
		}
		addGauges(cur)
		prev = cur
	}

	if intervals == 0 {
		return nil, fmt.Errorf("no /proc sweep after the first one succeeded, so no CPU window was measured")
	}
	var out []procUsage
	for g, runs := range rss {
		rates := cpuRates[g]
		for len(rates) < intervals {
			rates = append(rates, 0)
		}
		out = append(out, procUsage{
			Name:     g,
			CPUMilli: median(rates),
			RSSMiB:   median(runs),
			Procs:    procs[g],
			CPURates: rates,
			RSSRuns:  runs,
		})
	}
	sortUsage(out)
	return out, nil
}
