package stress

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestCaller() *rawCaller {
	return &rawCaller{counts: map[string]int{}, maxLatency: time.Second}
}

func TestRawCallerAcceptsTheContract(t *testing.T) {
	c := newTestCaller()
	c.record("read", rawAnswer{status: 200})
	c.record("create", rawAnswer{status: 202})
	c.record("create", rawAnswer{status: 409})
	c.record("read", rawAnswer{status: 503, retryAfter: "5"})
	if err := c.verdict(); err != nil {
		t.Fatalf("a compliant set of answers was rejected: %v", err)
	}
	if !c.sawStoreDown() {
		t.Error("a sampled 503 did not register as the store being down")
	}
	if tally := c.tally(); !strings.Contains(tally, "read:503=1") || !strings.Contains(tally, "create:409=1") {
		t.Errorf("tally = %q", tally)
	}
}

func TestRawCallerRejectsA500(t *testing.T) {
	c := newTestCaller()
	c.record("create", rawAnswer{status: 500, body: "boom"})
	err := c.verdict()
	if err == nil {
		t.Fatal("a 500 while the store is unreachable was accepted")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the failure does not name the status: %v", err)
	}
}

func TestRawCallerRejectsA503WithoutRetryAfter(t *testing.T) {
	for _, hdr := range []string{"", "soon", "0.5"} {
		c := newTestCaller()
		c.record("read", rawAnswer{status: 503, retryAfter: hdr})
		if err := c.verdict(); err == nil {
			t.Errorf("Retry-After %q was accepted", hdr)
		}
	}
	c := newTestCaller()
	c.record("read", rawAnswer{status: 503, retryAfter: "12"})
	if err := c.verdict(); err != nil {
		t.Errorf("a well-formed Retry-After was refused: %v", err)
	}
}

func TestRawCallerRejectsAHang(t *testing.T) {
	c := newTestCaller()
	c.record("create", rawAnswer{transport: context.DeadlineExceeded, took: 60 * time.Second})
	err := c.verdict()
	if err == nil {
		t.Fatal("a call that hung out its whole budget was accepted")
	}
	if !strings.Contains(err.Error(), "hung") {
		t.Errorf("the failure does not say the call hung: %v", err)
	}
	// A connection refused is the store being gone, not a contract breach.
	c2 := newTestCaller()
	c2.record("read", rawAnswer{transport: errors.New("connection refused"), took: time.Second})
	if err := c2.verdict(); err != nil {
		t.Errorf("a refused connection was treated as a contract breach: %v", err)
	}
	if !c2.sawStoreDown() {
		t.Error("a transport error did not register as the store being down")
	}
}

func TestRawCallerTracksAcceptedAndRefused(t *testing.T) {
	c := newTestCaller()
	c.mu.Lock()
	c.accepted = []string{"a"}
	c.refused = []string{"b"}
	c.mu.Unlock()
	c.record("create", rawAnswer{status: http.StatusAccepted})
	if len(c.accepted) != 1 || len(c.refused) != 1 {
		t.Errorf("record must not touch the accepted/refused lists: %v %v", c.accepted, c.refused)
	}
}
