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

type procUsage struct {
	Name     string
	CPUMilli float64
	RSSMiB   float64
	Procs    int
}

type procSample struct {
	At        time.Time
	PageBytes float64
	Ticks     map[string]float64
	Pages     map[string]float64
	Counts    map[string]int
}

// comm is parenthesised and may contain spaces, so fields are counted from after the ")": utime and stime 12th and 13th, rss 22nd.
func parseProcStat(out string, at time.Time) procSample {
	s := procSample{At: at, PageBytes: defaultPageBytes, Ticks: map[string]float64{}, Pages: map[string]float64{}, Counts: map[string]int{}}
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
	cpuSeconds := map[string]float64{}
	rssSum := map[string]float64{}
	rssN := map[string]int{}
	procs := map[string]int{}
	var window float64
	addGauges := func(s procSample) {
		for g, pages := range s.Pages {
			rssSum[g] += pages * s.PageBytes / (1 << 20)
			rssN[g]++
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
			window += dt
			for g, ticks := range cur.Ticks {
				if d := ticks - prev.Ticks[g]; d >= 0 {
					cpuSeconds[g] += d / clockTicks
				}
			}
		}
		addGauges(cur)
		prev = cur
	}

	if window <= 0 {
		return nil, fmt.Errorf("no /proc sweep after the first one succeeded, so no CPU window was measured")
	}
	var out []procUsage
	for g, n := range rssN {
		out = append(out, procUsage{
			Name:     g,
			CPUMilli: cpuSeconds[g] / window * 1000,
			RSSMiB:   rssSum[g] / float64(n),
			Procs:    procs[g],
		})
	}
	sortUsage(out)
	return out, nil
}
