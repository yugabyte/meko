package mekoload

import (
	"encoding/base64"
	"sort"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// OpAgg is one op summed over every pod. Latency comes from the merged
// histograms, or, when a pod sent none, from the worst of the pods.
type OpAgg struct {
	Count         int64
	OK            int64
	Errors        int64
	ErrorRate     float64
	Throughput    float64
	OKThroughput  float64
	ErrorsByClass map[string]int64
	Statuses      map[string]int64
	Latency       Latency
}

// Aggregate is the runs of every pod of a Load as one result.
type Aggregate struct {
	Pods         []Stats
	States       map[string]int
	Elapsed      float64
	Total        OpAgg
	Ops          map[string]OpAgg
	Http5xx      int64
	Invariants   map[string]Invariant
	Violations   int64
	Resources    ResourceCounts
	ErrorSamples map[string][]string
}

// Finished is true when every pod's run is done or failed.
func (a Aggregate) Finished() bool {
	for _, p := range a.Pods {
		if !p.Finished() {
			return false
		}
	}
	return len(a.Pods) > 0
}

// Failed lists the pods whose run failed, with the reason.
func (a Aggregate) Failed() []string {
	var out []string
	for _, p := range a.Pods {
		if p.State == "failed" {
			out = append(out, p.ID+": "+p.FailError)
		}
	}
	return out
}

// OpNames lists the ops, sorted.
func (a Aggregate) OpNames() []string {
	out := make([]string, 0, len(a.Ops))
	for n := range a.Ops {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Cleanups collects the cleanup result of every pod that has one.
func (a Aggregate) Cleanups() []CleanupResult {
	var out []CleanupResult
	for _, p := range a.Pods {
		if p.Cleanup != nil {
			out = append(out, *p.Cleanup)
		}
	}
	return out
}

// Merge sums the pods' stats.
func Merge(stats []Stats) Aggregate {
	a := Aggregate{
		Pods: stats, States: map[string]int{}, Ops: map[string]OpAgg{},
		Invariants: map[string]Invariant{}, ErrorSamples: map[string][]string{},
	}
	opStats := map[string][]OpSummary{}
	var all []OpSummary
	hists := map[string][]*hdr.Histogram{}
	for _, s := range stats {
		a.States[s.State]++
		if s.Summary.ElapsedSeconds > a.Elapsed {
			a.Elapsed = s.Summary.ElapsedSeconds
		}
		a.Http5xx += s.Summary.Http5xx
		a.Violations += s.Violations
		a.Resources.DatapacksCreated += s.Resources.DatapacksCreated
		a.Resources.DatapacksDeleted += s.Resources.DatapacksDeleted
		a.Resources.DatapacksLive += s.Resources.DatapacksLive
		a.Resources.PATsCreated += s.Resources.PATsCreated
		a.Resources.PATsRevoked += s.Resources.PATsRevoked
		a.Resources.PATsLive += s.Resources.PATsLive
		for name, inv := range s.Invariants {
			cur := a.Invariants[name]
			cur.Checked += inv.Checked
			cur.Violated += inv.Violated
			for _, sm := range inv.Samples {
				if len(cur.Samples) < 5 {
					cur.Samples = append(cur.Samples, sm)
				}
			}
			a.Invariants[name] = cur
		}
		for k, v := range s.Summary.ErrorSamples {
			if len(a.ErrorSamples[k]) < 3 {
				a.ErrorSamples[k] = append(a.ErrorSamples[k], v...)
			}
		}
		all = append(all, s.Summary.Total)
		for op, sum := range s.Summary.Ops {
			opStats[op] = append(opStats[op], sum)
		}
		for op, enc := range s.HDR {
			if h := decodeHDR(enc); h != nil {
				hists[op] = append(hists[op], h)
			}
		}
	}
	for op, sums := range opStats {
		a.Ops[op] = mergeOp(sums, hists[op])
	}
	a.Total = mergeOp(all, hists[""])
	return a
}

func decodeHDR(enc string) *hdr.Histogram {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	h, err := hdr.Decode(raw)
	if err != nil {
		return nil
	}
	return h
}

func mergeOp(sums []OpSummary, hists []*hdr.Histogram) OpAgg {
	out := OpAgg{ErrorsByClass: map[string]int64{}, Statuses: map[string]int64{}}
	var weighted float64
	for _, s := range sums {
		out.Count += s.Count
		out.OK += s.OK
		out.Errors += s.Errors
		out.Throughput += s.Throughput
		out.OKThroughput += s.OKThroughput
		for k, v := range s.ErrorsByClass {
			out.ErrorsByClass[k] += v
		}
		for k, v := range s.Statuses {
			out.Statuses[k] += v
		}
		weighted += s.Latency.Mean * float64(s.OK)
		out.Latency.P50 = max(out.Latency.P50, s.Latency.P50)
		out.Latency.P95 = max(out.Latency.P95, s.Latency.P95)
		out.Latency.P99 = max(out.Latency.P99, s.Latency.P99)
		out.Latency.Max = max(out.Latency.Max, s.Latency.Max)
	}
	if out.Count > 0 {
		out.ErrorRate = float64(out.Errors) / float64(out.Count)
	}
	if out.OK > 0 {
		out.Latency.Mean = weighted / float64(out.OK)
	}
	if len(hists) > 0 && len(hists) >= len(sums) {
		m := hdr.New(1, 10*60*1_000_000, 3)
		for _, h := range hists {
			m.Merge(h)
		}
		if m.TotalCount() > 0 {
			ms := func(v int64) float64 { return float64(v) / 1000 }
			out.Latency = Latency{Mean: m.Mean() / 1000, P50: ms(m.ValueAtQuantile(50)), P95: ms(m.ValueAtQuantile(95)), P99: ms(m.ValueAtQuantile(99)), Max: ms(m.Max())}
		}
	}
	return out
}
