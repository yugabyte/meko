package stress

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/metrics"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	endpointPodSelector   = "app.kubernetes.io/name=yb-amp-endpoint"
	safekeeperMetricsPort = 7676
	pageserverMetricsPort = 9898
	controllerMetricsPort = 8080
)

type faultMetricsParams struct {
	Enabled    bool
	RiseFactor float64
}

func faultMetricsParamsFromConfig(c *core.Config) faultMetricsParams {
	return faultMetricsParams{
		Enabled:    c.GetBool("stress.fault_metrics.enabled", true),
		RiseFactor: c.GetFloat("stress.fault_metrics.rise_factor", 2.0),
	}
}

type faultMetricSpec struct {
	Title string
	Raw   string
	Means string
}

type faultMetricsWatch struct {
	sampler *metrics.Sampler
	params  faultMetricsParams
	src     metrics.Source
	specs   []faultMetricSpec

	mu    sync.Mutex
	start time.Time
	end   time.Time
}

func safekeeperFaultMetrics() (metrics.Source, []faultMetricSpec) {
	src := metrics.Source{
		Tag: "safekeeper", Selector: endpointPodSelector, Container: computeContainer,
		Port: safekeeperMetricsPort, Path: "/metrics",
		Collect: []metrics.Entry{
			{Match: `^safekeeper_wal_consumer_batch_retries_total$`, Title: "WAL consumer batch retries", Kind: metrics.KindCounter},
		},
	}
	return src, []faultMetricSpec{
		{Title: "WAL consumer batch retries", Raw: "safekeeper_wal_consumer_batch_retries_total",
			Means: "batch flushes into amp_storage.wals that failed and were retried, so WAL durability is not advancing"},
	}
}

func pageserverFaultMetrics() (metrics.Source, []faultMetricSpec) {
	src := metrics.Source{
		Tag: "pageserver", Selector: endpointPodSelector, Container: computeContainer,
		Port: pageserverMetricsPort, Path: "/metrics",
		Collect: []metrics.Entry{
			{Match: `^yb_client_errors_total$`, Title: "YB client errors", Kind: metrics.KindCounter},
			{
				Match: `^pageserver_yb_store_op_seconds_(count|sum)$`,
				Title: "YB store ops", Kind: metrics.KindLatency,
				LatencyTitle: "YB store op latency", SumUnit: "s",
			},
		},
	}
	return src, []faultMetricSpec{
		{Title: "YB client errors", Raw: "yb_client_errors_total",
			Means: "YugabyteDB client errors the pageserver classified and retried while the node was unreachable"},
		{Title: "YB store op latency", Raw: "pageserver_yb_store_op_seconds",
			Means: "how long a PageStore round trip to YugabyteDB takes"},
	}
}

func controllerFaultMetrics(t *core.T) (metrics.Source, []faultMetricSpec) {
	src := metrics.Source{
		Tag: "controller", Selector: "app=" + ampstack.FromConfig(t.Options).ControllerName, Container: "controller",
		Port: controllerMetricsPort, Path: "/metrics",
		Collect: []metrics.Entry{
			{Match: `^amp_controller_tasks_reclaimed_total$`, Title: "Tasks reclaimed", Kind: metrics.KindCounter},
		},
	}
	return src, []faultMetricSpec{
		{Title: "Tasks reclaimed", Raw: "amp_controller_tasks_reclaimed_total",
			Means: "tasks a controller took over from a predecessor that died or shut down while running them"},
	}
}

func startFaultMetrics(t *core.T, kube *k8s.Kubectl, src metrics.Source, specs []faultMetricSpec, logf func(string, ...any)) *faultMetricsWatch {
	params := faultMetricsParamsFromConfig(t.Options)
	if !params.Enabled {
		return nil
	}
	if kube == nil {
		return &faultMetricsWatch{params: params, src: src, specs: specs}
	}
	cfg := metrics.FromConfig(t.Options)
	cfg.Enabled = true
	s := metrics.NewSources(kube, cfg, src).WithLogf(logf)
	if err := s.Start(t.Context); err != nil {
		logf("fault_metrics: sampler not started, the step will report it: %v", err)
		return &faultMetricsWatch{params: params, src: src, specs: specs}
	}
	return &faultMetricsWatch{sampler: s, params: params, src: src, specs: specs}
}

// The window spans the first open to the last close: a per-round window would
// leave the next round's fault inside this round's settle tail.
func (w *faultMetricsWatch) Open() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.start.IsZero() {
		w.start = time.Now()
	}
}

func (w *faultMetricsWatch) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.end = time.Now()
}

func (w *faultMetricsWatch) stop() {
	if w == nil || w.sampler == nil {
		return
	}
	w.sampler.Stop()
}

func (w *faultMetricsWatch) Assert(t *core.T) error {
	if w == nil {
		return nil
	}
	return t.Step("fault_metrics", func(s *core.StepHandle) error {
		if w.sampler == nil {
			return fmt.Errorf("the %s metrics sampler never started, so nothing can be said about whether the product's metrics showed the fault", w.src.Tag)
		}
		w.mu.Lock()
		start, end := w.start, w.end
		w.mu.Unlock()
		if start.IsZero() || end.IsZero() || !end.After(start) {
			return fmt.Errorf("no fault window was recorded (%v to %v), so the metrics have nothing to be judged against", start, end)
		}
		series := w.sampler.Stop()
		for _, ser := range series {
			t.MetricSeries(core.Metric{
				Key: ser.Key, Value: ser.Average(), Unit: ser.Unit, RoundVal: ser.Round,
				Raw: ser.Raw, Desc: ser.Desc, Series: ser.Pairs(),
			})
		}
		if len(series) == 0 {
			return fmt.Errorf("the %s exposition was never scraped: no pod matching %s answered :%d%s, so the run cannot tell a missing product metric from a missing probe",
				w.src.Tag, w.src.Selector, w.src.Port, w.src.Path)
		}
		s.Set("window", end.Sub(start).Round(time.Second).String())
		s.Set("rise_factor", w.params.RiseFactor)

		var missing, blind, flat []string
		for _, spec := range w.specs {
			ser, ok := faultSeries(series, "["+w.src.Tag+"] "+spec.Title)
			if !ok {
				missing = append(missing, fmt.Sprintf("%s (%s)", spec.Raw, spec.Means))
				continue
			}
			m := measureWindow(ser, start, end)
			s.Set(spec.Raw+"_baseline", round4(m.baseline))
			s.Set(spec.Raw+"_baseline_samples", m.pre)
			s.Set(spec.Raw+"_peak", round4(m.peak))
			s.Set(spec.Raw+"_settled", round4(m.settled))
			if m.pre == 0 {
				blind = append(blind, fmt.Sprintf("%s carries no sample before the fault, so a rise over its baseline would mean nothing (%s)", spec.Raw, spec.Means))
				continue
			}
			if !m.rose(w.params.RiseFactor) {
				flat = append(flat, fmt.Sprintf("%s stayed at %g during the fault against a %g baseline (%s)",
					spec.Raw, round4(m.peak), round4(m.baseline), spec.Means))
				continue
			}
			if !m.recovered(w.params.RiseFactor) {
				flat = append(flat, fmt.Sprintf("%s peaked at %g and is still %g after the fault, over the %g baseline (%s)",
					spec.Raw, round4(m.peak), round4(m.settled), round4(m.baseline), spec.Means))
			}
		}
		var why []string
		if len(missing) > 0 {
			why = append(why, "its exposition carries no series named "+strings.Join(missing, ", ")+
				", so no alert can be written on this fault at all")
		}
		if len(blind) > 0 {
			why = append(why, strings.Join(blind, "; "))
		}
		if len(flat) > 0 {
			why = append(why, strings.Join(flat, "; "))
		}
		if len(why) > 0 {
			return fmt.Errorf("the %s's own metrics do not show the fault the test injected over the %s window: %s",
				w.src.Tag, end.Sub(start).Round(time.Second), strings.Join(why, "; and "))
		}
		return nil
	})
}

func faultSeries(series []metrics.Series, key string) (metrics.Series, bool) {
	for _, ser := range series {
		if ser.Key == key {
			return ser, true
		}
	}
	return metrics.Series{}, false
}

type windowStat struct {
	baseline float64
	peak     float64
	settled  float64
	pre      int
}

// Peak, not mean: a restart mid-window resets the process's own gauges.
func measureWindow(ser metrics.Series, start, end time.Time) windowStat {
	var st windowStat
	var preSum, postSum float64
	var pre, post int
	s, e := start.UnixMilli(), end.UnixMilli()
	for _, pts := range ser.Lines {
		for _, p := range pts {
			switch {
			case p.UnixMS < s:
				preSum += p.Value
				pre++
			case p.UnixMS <= e:
				st.peak = math.Max(st.peak, p.Value)
			default:
				postSum += p.Value
				post++
			}
		}
	}
	st.pre = pre
	if pre > 0 {
		st.baseline = preSum / float64(pre)
	}
	if post > 0 {
		st.settled = postSum / float64(post)
	}
	return st
}

func (w windowStat) rose(factor float64) bool {
	return w.peak > 0 && w.peak >= w.baseline*factor
}

// Back to baseline, or down by the factor it rose by. A zero baseline only ever
// clears the second.
func (w windowStat) recovered(factor float64) bool {
	return w.settled <= w.baseline*factor || w.settled*factor <= w.peak
}

func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}
