package storagecontroller

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/controller/internal/config"
	"github.com/yugabyte/yb-amp/controller/internal/process"
)

// These integration tests stand a fake storage controller in for the real Rust
// binary by re-executing this test binary in "fake" mode (the os/exec helper
// pattern). The fake honors the same --listen the real one does, serves /ready,
// records its PID, and exits cleanly on SIGTERM — enough to exercise the real
// Spec (args + readiness probe), the supervisor, and the manager wiring that
// cmd/controller/main.go uses, without compiling storage_controller.

const (
	fakeEnvVar = "AMP_SC_FAKE"
	pidEnvVar  = "AMP_SC_PIDFILE"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnvVar) == "1" {
		runFakeStorageController()
		return
	}
	os.Exit(m.Run())
}

func runFakeStorageController() {
	var addr string
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--listen" {
			addr = args[i+1]
		}
	}
	if pf := os.Getenv(pidEnvVar); pf != "" {
		_ = os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: addr, Handler: mux}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	case err := <-errc:
		// Failed to bind/serve before any signal: exit non-zero so the
		// supervisor treats it as a crash and restarts us.
		if err != nil && err != http.ErrServerClosed {
			os.Exit(1)
		}
	}
}

func TestManagedStorageControllerStartsAndRestarts(t *testing.T) {
	addr := freePort(t)
	pidfile := filepath.Join(t.TempDir(), "sc.pid")

	spec := Spec(config.StorageControllerConfig{
		Enabled:      true,
		BinPath:      "storage_controller",
		ListenAddr:   addr,
		DatabaseURL:  "postgres://example/db",
		Dev:          true,
		StartTimeout: 5 * time.Second,
		StopTimeout:  2 * time.Second,
	})
	// Run this test binary as the fake storage controller, keeping the real
	// args, readiness client, and supervisor. Fast restart for the test.
	spec.Path = os.Args[0]
	spec.Env = []string{fakeEnvVar + "=1", pidEnvVar + "=" + pidfile}
	spec.Restart = process.Backoff{Initial: 10 * time.Millisecond, Max: 100 * time.Millisecond, ResetAfter: time.Hour}

	// Mirror cmd/controller/main.go: register the spec with a Manager and run it.
	mgr := process.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup := mgr.Add(spec)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); mgr.Run(ctx) }()

	// 1. On init, controller starts the storage controller and it serves /ready.
	select {
	case <-sup.Ready():
	case <-time.After(8 * time.Second):
		cancel()
		t.Fatal("managed storage controller never became ready on init")
	}
	client := NewClient(addr)
	if err := client.Ready(ctx); err != nil {
		cancel()
		t.Fatalf("Ready via client: %v", err)
	}
	pid1 := waitPid(t, pidfile, 0)

	// 2. Kill it; the supervisor must restart it with a new PID and it serves again.
	if err := syscall.Kill(pid1, syscall.SIGKILL); err != nil {
		cancel()
		t.Fatalf("kill %d: %v", pid1, err)
	}
	pid2 := waitPid(t, pidfile, pid1)
	if pid2 <= 0 || pid2 == pid1 {
		cancel()
		t.Fatalf("expected a restart with a new pid, got %d (was %d)", pid2, pid1)
	}
	waitReady(t, client)

	// 3. Clean shutdown when the controller stops.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Run did not return after cancel")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitPid(t *testing.T, path string, exclude int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && pid != exclude {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no new pid in %s (excluding %d) within timeout", path, exclude)
	return 0
}

func waitReady(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.Ready(context.Background()); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("storage controller did not return to ready after restart")
}
