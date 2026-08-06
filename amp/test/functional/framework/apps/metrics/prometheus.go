package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// promSample is the previous reading of one prometheus series on one pod.
type promSample struct {
	value float64
	at    time.Time
}

// promCmd fetches the exposition text over the pod's own loopback. The YB image
// is not guaranteed to carry curl, so busybox wget is the fallback; the comment
// lines are dropped in-pod so they never cross the exec.
func promCmd(port int) string {
	url := fmt.Sprintf("http://localhost:%d/prometheus-metrics", port)
	return fmt.Sprintf("(curl -sf %s || wget -qO- %s) 2>/dev/null | grep -v '^#'", url, url)
}

// parseExposition parses prometheus exposition lines into name → value, keeping
// only names matching one of include. Label dimensions are aggregated away by
// summing every line sharing a name (a YB server reports one line per
// tablet/table); the per-pod split is what the caller keeps. Unparsable lines
// and non-finite values are skipped.
func parseExposition(text string, include []*regexp.Regexp) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if name == "" || !matchAny(name, include) {
			continue
		}
		rest := line[len(name):]
		if strings.HasPrefix(rest, "{") {
			j := strings.LastIndex(rest, "}")
			if j < 0 {
				continue
			}
			rest = rest[j+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || v != v {
			continue
		}
		out[name] += v
	}
	return out
}

func matchAny(name string, patterns []*regexp.Regexp) bool {
	for _, re := range patterns {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// isCounter decides how a series is reported: a counter as a per-second rate from
// its delta, anything else as a gauge passed through. Prometheus histogram and
// summary components (_count/_sum) always count; the rest is declared by
// apps.metrics.counters, NOT guessed from whether the value grows — a YB gauge
// like mem_tracker or total_sst_files_size also rises under load, and reporting
// one of those as bytes/s would be a lie.
func isCounter(name string, counters []*regexp.Regexp) bool {
	if strings.HasSuffix(name, "_count") || strings.HasSuffix(name, "_sum") {
		return true
	}
	return matchAny(name, counters)
}

// samplePrometheus scrapes one pod's /prometheus-metrics and converts the kept
// values into readings.
func samplePrometheus(
	ctx context.Context, ex runner, port int,
	include, counters []*regexp.Regexp, st *podState, now time.Time, logf func(string, ...any),
) []reading {
	res, err := ex.Run(ctx, promCmd(port))
	if err != nil {
		logf("metrics: prometheus probe :%d failed: %v", port, err)
		return nil
	}
	if strings.TrimSpace(res.Stdout) == "" {
		logf("metrics: prometheus probe :%d returned nothing (neither curl nor wget produced output)", port)
		return nil
	}
	values := parseExposition(res.Stdout, include)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]reading, 0, len(names))
	for _, name := range names {
		if rd, ok := derive(name, values[name], isCounter(name, counters), now, st); ok {
			out = append(out, rd)
		}
	}
	return out
}

// derive turns one scraped value into a reading, updating the pod's memory of the
// series. A counter yields a per-second rate, so it produces nothing on its first
// sample or across a reset (the value went backwards); a gauge passes through.
func derive(name string, v float64, counter bool, now time.Time, st *podState) (reading, bool) {
	prev, seen := st.prom[name]
	st.prom[name] = promSample{value: v, at: now}
	if !counter {
		return reading{name: name, round: 2, value: v, at: now}, true
	}
	if !seen {
		return reading{}, false
	}
	dt := now.Sub(prev.at).Seconds()
	delta := v - prev.value
	if dt <= 0 || delta < 0 {
		return reading{}, false
	}
	return reading{name: name, unit: "/s", round: 2, value: delta / dt, at: now}, true
}
