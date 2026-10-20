package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"meko/ycp/internal/api"
	"meko/ycp/internal/ycpclient"
)

// desiredServer answers every desired-state read with body, counting the reads. With
// hold set it holds a long-poll for the wait it asked for, as a control plane whose
// token never moves would.
func desiredServer(t *testing.T, hold bool, body map[string]any, status int) (*httptest.Server, *int32) {
	t.Helper()
	var reads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reads, 1)
		if wait, err := strconv.Atoi(r.URL.Query().Get("wait")); hold && err == nil {
			time.Sleep(time.Duration(wait) * time.Second)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
	}))
	t.Cleanup(srv.Close)
	return srv, &reads
}

func longPollAgent(srv *httptest.Server, last api.DesiredState) *agent {
	return &agent{ycp: ycpclient.New(srv.URL, "key", srv.Client()), lastGood: last, haveGood: true}
}

// A moved token wakes the loop, and the tick it starts reads the state the hold
// returned rather than fetching it again.
func TestWaitChange_AChangeIsHandedToTheNextDesired(t *testing.T) {
	srv, reads := desiredServer(t, false, map[string]any{
		"change_token": 8, "long_poll": true,
		"resources": []map[string]any{{"kind": "agent", "name": "a"}},
	}, http.StatusOK)
	a := longPollAgent(srv, api.DesiredState{ChangeToken: 7})

	if !a.WaitChange(context.Background(), 2*time.Second) {
		t.Fatal("WaitChange = false for a moved token")
	}
	ds, fresh, err := a.Desired(context.Background())
	if err != nil || !fresh || ds.ChangeToken != 8 || len(ds.Resources) != 1 {
		t.Fatalf("Desired = %+v fresh=%v err=%v, want the held answer as a fresh read", ds, fresh, err)
	}
	if n := atomic.LoadInt32(reads); n != 1 {
		t.Errorf("%d reads, want 1: the tick fetched again", n)
	}
	if _, _, _ = a.Desired(context.Background()); atomic.LoadInt32(reads) != 2 {
		t.Error("the held answer was handed out twice")
	}
}

// Nothing changed: holds repeat until d is spent, then the tick runs as usual. An
// interval longer than the control plane's cap takes several.
func TestWaitChange_UnchangedHoldsUntilTheTickIsDue(t *testing.T) {
	defer func(old time.Duration) { longPollMaxHold = old }(longPollMaxHold)
	longPollMaxHold = time.Second
	srv, reads := desiredServer(t, true,
		map[string]any{"change_token": 7, "long_poll": true}, http.StatusOK)
	a := longPollAgent(srv, api.DesiredState{ChangeToken: 7})

	start := time.Now()
	if a.WaitChange(context.Background(), 2500*time.Millisecond) {
		t.Fatal("WaitChange = true for an unchanged token")
	}
	// Two 1s holds; the last 0.5s is under a whole second, so the tick waits it out.
	if took := time.Since(start); took < 2*time.Second {
		t.Errorf("returned after %s, want the two whole-second holds", took)
	}
	if n := atomic.LoadInt32(reads); n < 2 {
		t.Errorf("%d reads, want a second hold after the first ran its course", n)
	}
}

// A control plane that predates long-polling answers at once without long_poll. The
// agent falls back to one plain read per tick until the re-probe, and still acts on
// an answer whose revision moved.
func TestWaitChange_AnOldControlPlaneFallsBackToTicking(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision int
		want     bool
	}{{"unchanged", 3, false}, {"revision moved", 4, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, reads := desiredServer(t, false, map[string]any{"revision": tc.revision}, http.StatusOK)
			a := longPollAgent(srv, api.DesiredState{Revision: 3})

			if got := a.WaitChange(context.Background(), 2*time.Second); got != tc.want {
				t.Errorf("WaitChange = %v, want %v", got, tc.want)
			}
			if a.WaitChange(context.Background(), 2*time.Second) {
				t.Error("a second wait inside the re-probe window reported a change")
			}
			if n := atomic.LoadInt32(reads); n != 1 {
				t.Errorf("%d reads, want 1: long-polling must stay off until the re-probe", n)
			}
		})
	}
}

// A failed hold (a proxy cutting it, a 5xx) waits out the tick and pauses long-polling.
func TestWaitChange_AFailedHoldWaitsOutTheTick(t *testing.T) {
	srv, reads := desiredServer(t, false, map[string]any{}, http.StatusGatewayTimeout)
	a := longPollAgent(srv, api.DesiredState{ChangeToken: 7})

	if a.WaitChange(context.Background(), 2*time.Second) {
		t.Fatal("WaitChange = true for a failed read")
	}
	if a.WaitChange(context.Background(), 2*time.Second) || atomic.LoadInt32(reads) != 1 {
		t.Errorf("%d reads, want long-polling paused after a failure", atomic.LoadInt32(reads))
	}
}

// With no state read yet there is no token to wait on: the tick reads plainly.
func TestWaitChange_NothingToCompareAgainst(t *testing.T) {
	srv, reads := desiredServer(t, false, map[string]any{"change_token": 8, "long_poll": true}, http.StatusOK)
	a := longPollAgent(srv, api.DesiredState{})
	a.haveGood = false

	if a.WaitChange(context.Background(), 2*time.Second) || atomic.LoadInt32(reads) != 0 {
		t.Error("long-polled with no last-good state")
	}
}

// A hop that drops since/wait gets the server's immediate answer, long_poll and all.
// Read as unchanged, that would mean two reads per tick, silently, forever; it has to
// pause long-polling like the other fallbacks.
func TestWaitChange_AnUnheldUnchangedAnswerPauses(t *testing.T) {
	srv, reads := desiredServer(t, false, map[string]any{"change_token": 7, "long_poll": true}, http.StatusOK)
	a := longPollAgent(srv, api.DesiredState{ChangeToken: 7})

	if a.WaitChange(context.Background(), 2*time.Second) {
		t.Fatal("WaitChange = true for an unchanged token")
	}
	if a.WaitChange(context.Background(), 2*time.Second) || atomic.LoadInt32(reads) != 1 {
		t.Errorf("%d reads, want long-polling paused after an unheld answer", atomic.LoadInt32(reads))
	}
}

// A hop that stalls instead of failing must not hold the tick past d by the HTTP
// client's whole timeout: the request is cut at d plus the slack.
func TestWaitChange_AStalledHoldIsCutAtTheTick(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	a := &agent{
		ycp:      ycpclient.New(srv.URL, "key", &http.Client{Timeout: time.Minute}),
		lastGood: api.DesiredState{ChangeToken: 7}, haveGood: true,
	}
	start := time.Now()
	if a.WaitChange(context.Background(), 2*time.Second) {
		t.Fatal("WaitChange = true for a stalled read")
	}
	if took := time.Since(start); took < time.Second || took > 2*time.Second+longPollSlack+time.Second {
		t.Errorf("WaitChange took %s, want it cut near 2s + %s", took, longPollSlack)
	}
}

// wait goes out in whole seconds, so with under a second left there is no hold worth
// asking for: the tick waits the remainder out without a read.
func TestWaitChange_UnderASecondLeftSkipsTheHold(t *testing.T) {
	srv, reads := desiredServer(t, true, map[string]any{"change_token": 7, "long_poll": true}, http.StatusOK)
	a := longPollAgent(srv, api.DesiredState{ChangeToken: 7})

	if a.WaitChange(context.Background(), 900*time.Millisecond) || atomic.LoadInt32(reads) != 0 {
		t.Errorf("held a read with 900ms left (%d reads)", atomic.LoadInt32(reads))
	}
}
