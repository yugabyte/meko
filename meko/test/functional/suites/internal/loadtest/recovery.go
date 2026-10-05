package loadtest

import (
	"fmt"
	"sort"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// Second is what every pod together did in one second.
type Second struct{ OK, Err int64 }

// Seconds sums the pods' timelines by second.
func Seconds(tls []mekoload.PodTimeline) map[int64]Second {
	out := map[int64]Second{}
	for _, tl := range tls {
		for _, b := range tl.Buckets {
			s := out[b.Unix]
			s.OK += b.OK
			s.Err += b.Err
			out[b.Unix] = s
		}
	}
	return out
}

// Recovered says how long after killAt the load was healthy again. Healthy is
// a window of consecutive seconds that each carried successful calls and
// together had an error rate of at most maxErr. The window has to end before
// until, which is the next fault or the end of the run. The second return is
// false when no such window exists.
func Recovered(sec map[int64]Second, killAt, until time.Time, window int, maxErr float64) (time.Duration, bool) {
	start := killAt.Unix()
	end := until.Unix()
	for s := start; s+int64(window) <= end; s++ {
		var ok, errs int64
		healthy := true
		for i := int64(0); i < int64(window); i++ {
			b, present := sec[s+i]
			if !present || b.OK == 0 {
				healthy = false
				break
			}
			ok += b.OK
			errs += b.Err
		}
		if healthy && float64(errs)/float64(ok+errs) <= maxErr {
			d := time.Unix(s, 0).Sub(killAt)
			return max(d, 0), true
		}
	}
	return 0, false
}

// Damage is the error rate in the 30 seconds after killAt.
func Damage(sec map[int64]Second, killAt time.Time) (rate float64, errs int64) {
	var ok int64
	for s := killAt.Unix(); s < killAt.Unix()+30; s++ {
		b := sec[s]
		ok += b.OK
		errs += b.Err
	}
	if ok+errs == 0 {
		return 0, 0
	}
	return float64(errs) / float64(ok+errs), errs
}

// Report records the load's numbers on the result as metrics, one set per op.
func Report(t *core.T, label string, agg mekoload.Aggregate) {
	t.Metric(label+".calls", float64(agg.Total.Count), "calls")
	t.Metric(label+".error_rate", agg.Total.ErrorRate*100, "%")
	t.Metric(label+".throughput", agg.Total.Throughput, "ops/s")
	t.Metric(label+".http_5xx", float64(agg.Http5xx), "calls")
	names := agg.OpNames()
	sort.Strings(names)
	for _, op := range names {
		o := agg.Ops[op]
		if o.OK == 0 {
			continue
		}
		t.Metric(fmt.Sprintf("%s.%s.p50_ms", label, op), o.Latency.P50, "ms")
		t.Metric(fmt.Sprintf("%s.%s.p99_ms", label, op), o.Latency.P99, "ms")
		t.Metric(fmt.Sprintf("%s.%s.error_rate", label, op), o.ErrorRate*100, "%")
	}
}
