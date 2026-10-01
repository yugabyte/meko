package metrics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The exposition paths the two sampled server families serve:
// YugabyteDB on its webserver port, the amp pageserver on its HTTP port
// (compute/pageserver/src/http/routes.rs).
const (
	ybPromPath         = "/prometheus-metrics"
	pageserverPromPath = "/metrics"
)

// promSample is the previous reading of one prometheus series on one pod.
type promSample struct {
	value float64
	at    time.Time
}

// promCmd fetches the exposition text over the pod's own loopback. The YB images
// carry curl and busybox images carry wget, but the amp endpoint image is bare
// Debian with neither binary (libcurl4 is the library, not the tool) — there the
// last resort is bash's /dev/tcp, with the HTTP headers stripped at the first
// blank line. Comment lines are KEPT: # HELP carries the server's own metric
// description and # TYPE its counter/gauge kind, both worth reading.
func promCmd(port int, path string) string {
	url := fmt.Sprintf("http://localhost:%d%s", port, path)
	devTCP := fmt.Sprintf(
		"bash -c 'exec 3<>/dev/tcp/localhost/%d && printf \"GET %s HTTP/1.0\\r\\nHost: localhost\\r\\nConnection: close\\r\\n\\r\\n\" >&3 && cat <&3' | sed '1,/^\\r\\{0,1\\}$/d'",
		port, path)
	return fmt.Sprintf("(curl -sf %s || wget -qO- %s || %s) 2>/dev/null", url, url, devTCP)
}

// promMeta is what the exposition's comment lines say about a metric: the
// server's own description (# HELP) and whether it is a counter (# TYPE).
type promMeta struct {
	desc    string
	counter bool
}

// parseExpositionMeta reads the # HELP / # TYPE lines for the matched entries.
func parseExpositionMeta(text string, entries []compiledEntry) map[string]promMeta {
	out := map[string]promMeta{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "# HELP ")
		if ok {
			if name, desc, found := strings.Cut(rest, " "); found {
				if _, want := matchEntry(name, entries); want {
					m := out[name]
					m.desc = strings.TrimSpace(desc)
					out[name] = m
				}
			}
			continue
		}
		if rest, ok = strings.CutPrefix(line, "# TYPE "); ok {
			if name, typ, found := strings.Cut(rest, " "); found {
				if _, want := matchEntry(name, entries); want {
					m := out[name]
					m.counter = strings.TrimSpace(typ) == "counter"
					out[name] = m
				}
			}
		}
	}
	return out
}

// parseExposition parses prometheus exposition lines into name → value, keeping
// only names matched by one of entries. Label dimensions are aggregated away by
// summing every line sharing a name (a YB server reports one line per
// tablet/table); the per-pod split is what the caller keeps. Unparsable lines
// and non-finite values are skipped.
func parseExposition(text string, entries []compiledEntry) map[string]float64 {
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
		if name == "" {
			continue
		}
		if _, ok := matchEntry(name, entries); !ok {
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

// matchEntry returns the first entry whose regex matches name.
func matchEntry(name string, entries []compiledEntry) (Entry, bool) {
	for _, ce := range entries {
		if ce.re.MatchString(name) {
			return ce.entry, true
		}
	}
	return Entry{}, false
}

// splitHist splits a histogram component name into its base and _count/_sum
// suffix. A latency entry that matches anything else contributes nothing —
// there is no pair to derive from a lone value.
func splitHist(name string) (base, suffix string, ok bool) {
	for _, s := range []string{"_count", "_sum"} {
		if b, cut := strings.CutSuffix(name, s); cut {
			return b, s, true
		}
	}
	return "", "", false
}

// sumToMS converts a histogram _sum's time unit into milliseconds. An unknown
// unit is treated as microseconds — the YB handler-latency convention and this
// field's documented default.
func sumToMS(unit string) float64 {
	switch unit {
	case "ns":
		return 1e-6
	case "ms":
		return 1
	case "s":
		return 1e3
	default:
		return 1e-3
	}
}

// histPair accumulates one latency entry's _count and _sum for one scrape.
type histPair struct {
	entry     Entry
	countName string
	sumName   string
	count     float64
	sum       float64
	haveCount bool
	haveSum   bool
}

// samplePrometheus scrapes one pod's exposition endpoint and converts the kept
// values into readings, each already named by its human title.
func samplePrometheus(
	ctx context.Context, ex runner, port int, path string,
	entries []compiledEntry, st *podState, now time.Time, logf func(string, ...any),
) []reading {
	res, err := ex.Run(ctx, promCmd(port, path))
	if err != nil {
		logf("metrics: prometheus probe :%d%s failed: %v", port, path, err)
		return nil
	}
	if strings.TrimSpace(res.Stdout) == "" {
		logf("metrics: prometheus probe :%d%s returned nothing (neither curl nor wget produced output)", port, path)
		return nil
	}
	values := parseExposition(res.Stdout, entries)
	meta := parseExpositionMeta(res.Stdout, entries)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]reading, 0, len(names))
	pairs := map[string]*histPair{}
	var pairOrder []string
	for _, name := range names {
		e, ok := matchEntry(name, entries)
		if !ok {
			continue
		}
		if e.Kind == KindLatency {
			base, suffix, ok := splitHist(name)
			if !ok {
				continue
			}
			p := pairs[base]
			if p == nil {
				p = &histPair{entry: e}
				pairs[base] = p
				pairOrder = append(pairOrder, base)
			}
			if suffix == "_count" {
				p.count, p.countName, p.haveCount = values[name], name, true
			} else {
				p.sum, p.sumName, p.haveSum = values[name], name, true
			}
			continue
		}
		// The server says what it is: a # TYPE counter beats guessing, so a plain
		// entry the config did not classify still gets rate-converted.
		if e.Kind == KindGauge && meta[name].counter {
			e.Kind = KindCounter
		}
		if rd, ok := derive(name, values[name], e, now, st); ok {
			rd.desc = meta[name].desc
			out = append(out, rd)
		}
	}
	for _, base := range pairOrder {
		out = append(out, derivePair(pairs[base], now, st)...)
	}
	return out
}

// derive turns one scraped gauge or counter value into a reading, updating the
// pod's memory of the series. A counter yields a per-second rate, so it produces
// nothing without a base to measure from (see counterBase) or across a reset (the
// value went backwards); a gauge passes through.
//
// A reset is not re-based on zero the way Prometheus's rate() does it, because the
// value here is a SUM over label sets: a tserver's per-tablet counters drop when a
// tablet moves off it, with no restart, and re-basing that would read the whole
// remaining total as one interval's work.
func derive(name string, v float64, e Entry, now time.Time, st *podState) (reading, bool) {
	prev, seen := st.prom[name]
	st.prom[name] = promSample{value: v, at: now}
	rd := reading{name: e.Title, unit: e.Unit, round: e.Round, at: now, raw: []string{name}}
	if e.Kind != KindCounter {
		rd.value = v / e.DivideBy
		return rd, true
	}
	base, ok := counterBase(prev, seen, st)
	if !ok {
		return reading{}, false
	}
	dt := now.Sub(base.at).Seconds()
	delta := v - base.value
	if dt <= 0 || delta < 0 {
		return reading{}, false
	}
	rd.value = (delta / dt) / e.DivideBy
	return rd, true
}

// counterBase is the sample a counter's delta is measured from: the previous one
// or, on a pod that appeared after sampling began, zero as of podState.newSince —
// otherwise a replacement's first interval, often the one a fault test is judging,
// is lost. A pod already running when sampling began has neither, so its first
// sample only seeds.
func counterBase(prev promSample, seen bool, st *podState) (promSample, bool) {
	if seen {
		return prev, true
	}
	if !st.newSince.IsZero() {
		return promSample{at: st.newSince}, true
	}
	return promSample{}, false
}

// derivePair turns a histogram's _count/_sum into the two series a human
// actually reads: throughput = Δcount/Δt, and MEAN LATENCY = Δsum/Δcount
// converted to milliseconds. Reporting the two raw rates instead (what a naive
// include list does) hands the reader a "sum per second" that means nothing.
// Both need a base to measure from (see counterBase); a counter reset or an idle
// interval (no calls) yields no latency point rather than a division by zero.
func derivePair(p *histPair, now time.Time, st *podState) []reading {
	if p == nil || !p.haveCount || !p.haveSum {
		return nil
	}
	prevCount, seenCount := st.prom[p.countName]
	prevSum, seenSum := st.prom[p.sumName]
	st.prom[p.countName] = promSample{value: p.count, at: now}
	st.prom[p.sumName] = promSample{value: p.sum, at: now}
	baseCount, okCount := counterBase(prevCount, seenCount, st)
	baseSum, okSum := counterBase(prevSum, seenSum, st)
	if !okCount || !okSum {
		return nil
	}
	dt := now.Sub(baseCount.at).Seconds()
	dCount := p.count - baseCount.value
	dSum := p.sum - baseSum.value
	if dt <= 0 || dCount < 0 || dSum < 0 {
		return nil
	}
	e := p.entry
	raw := []string{p.countName, p.sumName}
	sort.Strings(raw)
	out := []reading{{
		name: e.Title, unit: e.Unit, round: e.Round,
		value: (dCount / dt) / e.DivideBy, at: now, raw: raw,
	}}
	if dCount > 0 {
		out = append(out, reading{
			name: e.LatencyTitle, unit: "ms", round: latencyRound,
			value: (dSum / dCount) * sumToMS(e.SumUnit), at: now, raw: raw,
		})
	}
	return out
}
