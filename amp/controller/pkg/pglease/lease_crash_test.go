package pglease

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fail-fast contract is the one thing an in-process test cannot prove: a
// recorded abort only shows that the hook was reached, not that the process
// stopped being able to write. So this file re-execs the test binary and lets
// the child die for real, under the actual AbortProcess.
//
// It needs no database: the loss is scripted by crashBackend, so the child
// neither dials anything nor sleeps and hopes.
const (
	// crashChildEnv marks the re-exec'd child. Without it set the test body is
	// the parent, so an ordinary `go test ./...` never runs the child half.
	crashChildEnv = "AMP_PGLEASE_CRASH_CHILD"
	crashModeEnv  = "AMP_PGLEASE_CRASH_MODE"
	crashKeyEnv   = "AMP_PGLEASE_CRASH_KEY"

	// crashSentinel is printed by the child once it holds the lease and
	// immediately before loss becomes possible. Asserting on it is what stops a
	// crash during child setup from reading as a pass: any exit before this
	// line means the child never got as far as holding a lease.
	crashSentinel = "PGLEASE-CRASH-CHILD-HOLDS-LEASE"

	// crashModeDeadline fails every renewal, so nothing says the lease is lost;
	// it just can no longer be proven held. crashModeLost has a renewal refused
	// outright.
	crashModeDeadline = "deadline"
	crashModeLost     = "lost"
)

// Child timings, per mode. Both renew at 500ms and are valid per
// Config.Validate. The deadline mode dies at TTL-SafetyMargin = 1.1s, after two
// failed attempts. The lost mode dies on the first tick, so its deadline is put
// far out of reach: the cause it asserts on must be the refusal itself and never
// a deadline that won a race on a loaded machine.
func crashChildConfig(mode string) Config {
	if mode == crashModeLost {
		return Config{
			TTL:          30 * time.Second,
			RenewEvery:   500 * time.Millisecond,
			RenewTimeout: 2 * time.Second,
			SafetyMargin: 5 * time.Second,
		}
	}
	return Config{
		TTL:          1500 * time.Millisecond,
		RenewEvery:   500 * time.Millisecond,
		RenewTimeout: 300 * time.Millisecond,
		SafetyMargin: 400 * time.Millisecond,
	}
}

// crashChildPatience bounds how long the child waits for the loss it was
// scripted to suffer. Generously above the 1.1s deadline so a loaded machine
// cannot fail it, and far below the child's own -test.timeout.
const crashChildPatience = 15 * time.Second

// crashLingerGrace is how long the child stays alive after observing the loss.
// lose() cancels the context before calling Abort, so a live process here means
// the abort returned instead of ending it — the exact failure the package
// exists to prevent, and it exits 1 (not AbortExitCode) so the parent can tell.
const crashLingerGrace = 2 * time.Second

// crashBackend grants the first acquire and then loses the lease the way its
// mode says. It is the child's whole environment: no database, no network.
type crashBackend struct {
	mode     string
	attempts atomic.Int64
}

func (b *crashBackend) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (Grant, error) {
	if b.attempts.Add(1) == 1 {
		return Grant{
			Granted:         true,
			ExpiresAtMicros: time.Now().Add(ttl).UnixMicro(),
			Holder:          owner,
		}, nil
	}
	switch b.mode {
	case crashModeLost:
		return Grant{Holder: "some-other-process", RetryAfter: time.Second}, nil
	default:
		return Grant{}, errors.New("crashBackend: renewal cannot be confirmed")
	}
}

func (b *crashBackend) Release(ctx context.Context, key, owner string) (bool, error) {
	return true, nil
}

// TestLeaseLossCrashesProcess proves a process that is alive but can no longer
// prove it holds the lease actually dies, so no progress is possible on a lost
// lease.
func TestLeaseLossCrashesProcess(t *testing.T) {
	if os.Getenv(crashChildEnv) == "1" {
		crashChild(t)
		return
	}
	if testing.Short() {
		t.Skip("re-execs the test binary")
	}

	cases := []struct {
		name      string
		mode      string
		wantCause string
	}{
		{"renewals never confirmed", crashModeDeadline, "renewal deadline passed with no confirmed renewal"},
		{"renewal refused after takeover", crashModeLost, "lease taken over by another owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("pglease-crash-%s-%d", tc.mode, os.Getpid())
			stdout, stderr, elapsed, err := runCrashChild(t, tc.mode, key)

			report := func(format string, args ...any) {
				t.Helper()
				t.Errorf(format, args...)
				t.Logf("child ran for %s\n--- child stdout ---\n%s\n--- child stderr ---\n%s",
					elapsed, stdout, stderr)
			}

			var exitErr *exec.ExitError
			switch {
			case err == nil:
				report("child exited 0: a lost lease must kill the process, not be survived")
				return
			case !errors.As(err, &exitErr):
				report("child did not exit with a status: %v", err)
				return
			}
			if got := exitErr.ExitCode(); got != AbortExitCode {
				// 1 is the child's own t.Fatal (it observed the loss and stayed
				// alive); 2 is a panic or runtime crash. Neither is a fence.
				report("child exit code = %d, want AbortExitCode %d", got, AbortExitCode)
				return
			}
			if !strings.Contains(stderr, crashSentinel) {
				report("child stderr lacks the %q sentinel, so it died before it ever held the lease", crashSentinel)
				return
			}
			// The fence must name the lease it lost: an operator reading only
			// this line has to know which resource was given up.
			wantLine := fmt.Sprintf("FATAL: pglease: lease %q lost", key)
			if !strings.Contains(stderr, wantLine) {
				report("child stderr lacks %q", wantLine)
			}
			if !strings.Contains(stderr, tc.wantCause) {
				report("child stderr lacks the cause %q", tc.wantCause)
			}
			if !strings.Contains(stderr, "goroutine ") {
				report("child stderr lacks a goroutine dump")
			}
		})
	}
}

// runCrashChild re-execs this binary as the child half of the test.
func runCrashChild(t *testing.T, mode, key string) (stdout, stderr string, elapsed time.Duration, err error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Anchored, and the top-level name rather than the subtest's: the child
	// branches on crashChildEnv before it ever reaches the subtests.
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestLeaseLossCrashesProcess$",
		"-test.count=1",
		"-test.timeout=30s",
	)
	cmd.Env = append(os.Environ(),
		crashChildEnv+"=1",
		crashModeEnv+"="+mode,
		crashKeyEnv+"="+key,
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// The child dies by os.Exit, so its pipes close with it; WaitDelay only
	// covers a wedged child holding them open past the context.
	cmd.WaitDelay = 10 * time.Second

	start := time.Now()
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), time.Since(start), err
}

// crashChild is the re-exec'd half: it holds a real lease over a backend that
// cannot renew it, and must be killed by the real AbortProcess. It never skips
// — a skipping child exits 0 and reads as a survived lease loss.
func crashChild(t *testing.T) {
	key := os.Getenv(crashKeyEnv)
	if key == "" {
		t.Fatalf("child started without %s", crashKeyEnv)
	}

	// Abort is left nil deliberately: the child must run the real AbortProcess,
	// which is the behaviour under test.
	mode := os.Getenv(crashModeEnv)
	client, err := New(&crashBackend{mode: mode}, NewOwner("pglease-crash-child"), crashChildConfig(mode))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	held, err := client.Acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("acquire %q: %v", key, err)
	}

	fmt.Fprintln(os.Stderr, crashSentinel)

	select {
	case <-held.Context().Done():
	case <-time.After(crashChildPatience):
		t.Fatalf("lease %q was never lost after %s: err=%v", key, crashChildPatience, held.Err())
	}

	time.Sleep(crashLingerGrace)
	t.Fatalf("still running %s after losing lease %q (%v): the abort returned instead of ending the process",
		crashLingerGrace, key, held.Err())
}
