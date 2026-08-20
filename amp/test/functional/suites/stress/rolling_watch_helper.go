// Proves an operation was actually rolling rather than trusting the option asked for; an all-at-once restart looks identical from the task's side.
package stress

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const DefaultRollingSampleInterval = 5 * time.Second

type rollingObservation struct {
	// Expected is the pod count from BEFORE the operation; a recreated pod vanishes from the live listing, so counting against that would misread "all ready".
	Expected int
	Samples  int
	MaxOut   int
	MinReady int
	// Err means the observation is incomplete, not proof.
	Err error
}

func (o rollingObservation) Complete() bool { return o.Err == nil && o.Samples > 0 && o.Expected > 0 }

func (o rollingObservation) String() string {
	return fmt.Sprintf("%d samples of %d serving pods: at most %d out at once, fewest ready %d",
		o.Samples, o.Expected, o.MaxOut, o.MinReady)
}

func watchRolling(ctx context.Context, kube *k8s.Kubectl, interval time.Duration, fn func() error) (rollingObservation, error) {
	if kube == nil {
		return rollingObservation{}, fn() // VM universe: nothing to sample
	}
	if interval <= 0 {
		interval = DefaultRollingSampleInterval
	}
	expected, err := countReadyTservers(ctx, kube)
	if err != nil {
		return rollingObservation{Err: err}, fn()
	}

	obs := rollingObservation{Expected: expected, MinReady: expected}
	var mu sync.Mutex
	sampleCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-t.C:
			}
			ready, err := countReadyTservers(sampleCtx, kube)
			mu.Lock()
			if err != nil {
				// A cancelled sample means the operation finished, not a failure.
				if sampleCtx.Err() == nil && obs.Err == nil {
					obs.Err = err
				}
				mu.Unlock()
				continue
			}
			obs.Samples++
			if out := expected - ready; out > obs.MaxOut {
				obs.MaxOut = out
			}
			if ready < obs.MinReady {
				obs.MinReady = ready
			}
			mu.Unlock()
		}
	}()

	// Inline to count the last samples; deferred so a panic in fn can't leave the sampler running.
	stopped := false
	stopSampler := func() {
		if stopped {
			return
		}
		stopped = true
		stop()
		<-done
	}
	defer stopSampler()

	runErr := fn()
	stopSampler()

	mu.Lock()
	defer mu.Unlock()
	return obs, runErr
}

func countReadyTservers(ctx context.Context, kube *k8s.Kubectl) (int, error) {
	var lastErr error
	for _, sel := range tserverSelectors {
		out, err := kube.GetSelector(ctx, "pod", sel,
			`{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[*].ready}{"\n"}{end}`)
		if err != nil {
			lastErr = err
			continue
		}
		ready, found := 0, false
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			found = true
			allReady := true
			for _, f := range fields[1:] {
				if f != "true" {
					allReady = false
					break
				}
			}
			if allReady {
				ready++
			}
		}
		if found {
			return ready, nil
		}
	}
	if lastErr != nil {
		return 0, fmt.Errorf("sample tserver readiness: %w", lastErr)
	}
	return 0, fmt.Errorf("no tserver pods matched any known selector in namespace %s", kube.Namespace())
}
