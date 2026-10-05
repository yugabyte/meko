package mekoload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
)

type podRun struct {
	pod *Pod
	id  string
}

// Load is one workload running on every pod of a fleet.
type Load struct {
	Workload string
	runs     []podRun
}

// StartRun starts workload on every pod, with config cfg(i) for pod i.
func (f *Fleet) StartRun(ctx context.Context, workload string, cfg func(i int) map[string]any, labels map[string]string) (*Load, error) {
	pods := f.Pods()
	l := &Load{Workload: workload, runs: make([]podRun, len(pods))}
	errs := make([]error, len(pods))
	var wg sync.WaitGroup
	for i, p := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.targetID == "" {
				errs[i] = fmt.Errorf("%s has no target, call CreateTargets first", p.Name)
				return
			}
			id, err := p.Client.CreateRun(ctx, workload, p.targetID, cfg(i), labels)
			l.runs[i] = podRun{pod: p, id: id}
			errs[i] = err
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		_ = l.Stop(ctx)
		return nil, fmt.Errorf("start %s: %w", workload, err)
	}
	return l, nil
}

func (l *Load) each(fn func(i int, r podRun) error) error {
	errs := make([]error, len(l.runs))
	var wg sync.WaitGroup
	for i, r := range l.runs {
		if r.id == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn(i, r)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Stats merges the pods' summaries. withHDR also asks for the latency
// histograms, which merge into exact percentiles.
func (l *Load) Stats(ctx context.Context, withHDR bool) (Aggregate, error) {
	stats := make([]Stats, len(l.runs))
	err := l.each(func(i int, r podRun) error {
		s, err := r.pod.Client.Stats(ctx, r.id, withHDR)
		if err != nil {
			return fmt.Errorf("%s: %w", r.pod.Name, err)
		}
		stats[i] = s
		return nil
	})
	if err != nil {
		return Aggregate{}, err
	}
	var present []Stats
	for _, s := range stats {
		if s.ID != "" {
			present = append(present, s)
		}
	}
	return Merge(present), nil
}

// Update merges cfg into every pod's live config.
func (l *Load) Update(ctx context.Context, cfg map[string]any) error {
	return l.each(func(_ int, r podRun) error { return r.pod.Client.Update(ctx, r.id, cfg) })
}

// Stop ends the load on every pod. The run cleans up afterwards if its config says so.
func (l *Load) Stop(ctx context.Context) error {
	return l.each(func(_ int, r podRun) error { return r.pod.Client.Stop(ctx, r.id) })
}

// Wait polls until every pod's run has finished or timeout passes, calling
// onPoll with each snapshot. A failed poll is retried, since a port-forward can blip.
func (l *Load) Wait(ctx context.Context, timeout, every time.Duration, onPoll func(Aggregate)) (Aggregate, error) {
	deadline := time.Now().Add(timeout)
	var last Aggregate
	var lastErr error
	for {
		agg, err := l.Stats(ctx, false)
		if err == nil {
			last, lastErr = agg, nil
			if onPoll != nil {
				onPoll(agg)
			}
			if agg.Finished() {
				return l.Stats(ctx, true)
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return last, fmt.Errorf("runs did not finish within %s, last poll: %w", timeout, lastErr)
			}
			return last, fmt.Errorf("runs did not finish within %s: states %v", timeout, last.States)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(every):
		}
	}
}

// Live is what each pod's run still holds.
func (l *Load) Live(ctx context.Context) ([]Live, error) {
	out := make([]Live, len(l.runs))
	err := l.each(func(i int, r podRun) error {
		v, err := r.pod.Client.Resources(ctx, r.id)
		out[i] = v
		return err
	})
	return out, err
}

// Cleanup runs the cleanup phase of every finished run.
func (l *Load) Cleanup(ctx context.Context) ([]CleanupResult, error) {
	out := make([]CleanupResult, len(l.runs))
	err := l.each(func(i int, r podRun) error {
		c, err := r.pod.Client.Cleanup(ctx, r.id)
		out[i] = c
		return err
	})
	return out, err
}

// PodTimeline is a pod's per-second timeline on the coordinator's clock.
type PodTimeline struct {
	Pod     string
	Buckets []Bucket
}

// Timelines reads every pod's per-second ok and error counts. Each pod's
// clock is compared with the coordinator's, so the buckets can be lined up
// with something the coordinator did.
func (l *Load) Timelines(ctx context.Context) ([]PodTimeline, error) {
	out := make([]PodTimeline, len(l.runs))
	err := l.each(func(i int, r podRun) error {
		before := time.Now()
		tl, err := r.pod.Client.Timeline(ctx, r.id)
		if err != nil {
			return fmt.Errorf("%s: %w", r.pod.Name, err)
		}
		mid := before.Add(time.Since(before) / 2)
		skew := time.Duration(tl.NowUnixMs-mid.UnixMilli()) * time.Millisecond
		shift := int64((skew + 500*time.Millisecond) / time.Second)
		for j := range tl.Buckets {
			tl.Buckets[j].Unix -= shift
		}
		out[i] = PodTimeline{Pod: r.pod.Name, Buckets: tl.Buckets}
		return nil
	})
	return out, err
}

// Collect attaches each pod's final summary, events and Prometheus text to sink.
func (l *Load) Collect(ctx context.Context, sink apps.ArtifactSink) error {
	return l.each(func(_ int, r podRun) error {
		var errs []error
		if s, err := r.pod.Client.Stats(ctx, r.id, false); err == nil {
			b, _ := json.MarshalIndent(s, "", "  ")
			errs = append(errs, sink.Add(r.pod.Name+"/"+l.Workload+"-summary.json", "json", b))
		} else {
			errs = append(errs, err)
		}
		if ev, err := r.pod.Client.Events(ctx, r.id); err == nil {
			errs = append(errs, sink.Add(r.pod.Name+"/"+l.Workload+"-events.json", "json", ev))
		}
		if m, err := r.pod.Client.Metrics(ctx); err == nil {
			errs = append(errs, sink.Add(r.pod.Name+"/metrics.txt", "log", []byte(m)))
		}
		return errors.Join(errs...)
	})
}
