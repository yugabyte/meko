package stress

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const controlPlaneProbeInterval = 5 * time.Second

// probeSample is one poll: when, and whether the controller answered 2xx.
type probeSample struct {
	at time.Time
	ok bool
}

// controlPlaneProbe polls one read endpoint of the controller over the client's
// own transport and keeps every sample. Status codes are never retried on that
// route, so a 500 during a backing fault is seen rather than retried away
// (meko#886); an error the transport has classified as the environment's (a dead
// port-forward, revive.go) is counted apart and never blamed on the controller.
type controlPlaneProbe struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	counts  map[int]int
	network int
	envErrs int
	samples []probeSample
	stop    chan struct{}
	done    chan struct{}
}

func newControlPlaneProbe(cl *controller.Client, poolID string) *controlPlaneProbe {
	return &controlPlaneProbe{
		url:    cl.BaseURL() + "/api/v1/storage-pools/" + poolID + "/clusters?limit=1",
		client: cl.HTTPClient(),
		counts: map[int]int{},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

func (p *controlPlaneProbe) Start(ctx context.Context) {
	go func() {
		defer close(p.done)
		t := time.NewTicker(controlPlaneProbeInterval)
		defer t.Stop()
		for {
			p.once(ctx)
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case <-t.C:
			}
		}
	}()
}

func (p *controlPlaneProbe) once(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, p.url, nil)
	if err != nil {
		return
	}
	resp, err := p.client.Do(req)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if err != nil {
		if core.IsEnvFailure(err) {
			p.envErrs++
			return
		}
		p.network++
		p.samples = append(p.samples, probeSample{now, false})
		return
	}
	resp.Body.Close()
	p.counts[resp.StatusCode]++
	p.samples = append(p.samples, probeSample{now, resp.StatusCode < 400})
}

// Abort ends the probe without waiting for anything.
func (p *controlPlaneProbe) Abort() {
	close(p.stop)
	<-p.done
}

// Stop waits for the first 2xx sampled after `after` (or the budget), then
// reports how long after `after` the controller last answered badly: 0 when no
// bad sample landed in the window, and measured=false when no 2xx was ever
// sampled in it.
func (p *controlPlaneProbe) Stop(after time.Time, budget time.Duration) (recovery time.Duration, measured bool) {
	deadline := after.Add(budget)
	for time.Now().Before(deadline) {
		if p.okSince(after) {
			break
		}
		time.Sleep(controlPlaneProbeInterval)
	}
	p.Abort()
	p.mu.Lock()
	defer p.mu.Unlock()
	var lastBad time.Time
	for _, s := range p.samples {
		if !s.at.After(after) {
			continue
		}
		if s.ok {
			measured = true
		} else {
			lastBad = s.at
		}
	}
	if !measured {
		return budget, false
	}
	if lastBad.IsZero() {
		return 0, true
	}
	return lastBad.Sub(after), true
}

func (p *controlPlaneProbe) okSince(after time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.samples) - 1; i >= 0; i-- {
		if !p.samples[i].at.After(after) {
			return false
		}
		if p.samples[i].ok {
			return true
		}
	}
	return false
}

// Summary returns the tally and the count of 5xx other than 503.
func (p *controlPlaneProbe) Summary() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := fmt.Sprintf("network_errors=%d env_errors=%d", p.network, p.envErrs)
	bad := 0
	for code, n := range p.counts {
		s += fmt.Sprintf(" http_%d=%d", code, n)
		if code >= 500 && code != http.StatusServiceUnavailable {
			bad += n
		}
	}
	return s, bad
}
