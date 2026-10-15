// Package health serves /healthz and /metrics.
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
)

// Heartbeat is beaten by the loop every tick; /healthz fails once it has stopped.
type Heartbeat struct {
	maxAge time.Duration
	last   atomic.Int64 // unix nanoseconds
}

// NewHeartbeat starts a heartbeat that goes stale after maxAge without a Beat.
func NewHeartbeat(maxAge time.Duration) *Heartbeat {
	h := &Heartbeat{maxAge: maxAge}
	h.Beat()
	return h
}

// Beat records that the loop is alive.
func (h *Heartbeat) Beat() { h.last.Store(time.Now().UnixNano()) }

// Alive reports whether the last Beat is recent enough.
func (h *Heartbeat) Alive() bool {
	return time.Since(time.Unix(0, h.last.Load())) <= h.maxAge
}

// Handler serves /healthz and /metrics.
func Handler(hb *Heartbeat) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !hb.Alive() {
			http.Error(w, "loop stalled", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.Handle("/metrics", promhttp.HandlerFor(stats.Registry, promhttp.HandlerOpts{}))
	return mux
}

// Serve listens on port until the returned shutdown function is called. A port of 0 or less serves nothing.
func Serve(port int, hb *Heartbeat) (shutdown func(context.Context) error, err error) {
	if port <= 0 {
		return func(context.Context) error { return nil }, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: Handler(hb), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server stopped", "error", err)
		}
	}()
	slog.Info("listening", "addr", ln.Addr().String(), "paths", "/healthz /metrics")
	return srv.Shutdown, nil
}
