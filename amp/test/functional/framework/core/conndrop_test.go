package core_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

func TestIsConnDropRequiresTheTypedMarker(t *testing.T) {
	drops := []error{
		&pg.TransportError{Err: errors.New("unexpected EOF")},
		fmt.Errorf(`exec "INSERT INTO cold_x (id, v) VALUES ($1, $2)": %w`,
			&pg.TransportError{Err: errors.New("unexpected EOF"), Addr: "127.0.0.1:5001"}),
	}
	for _, err := range drops {
		if !core.IsConnDrop(err) {
			t.Errorf("IsConnDrop(%v) = false, want true", err)
		}
	}

	// Text alone must not qualify: a body error quoting a workload log line
	// carries the words but not the pg client's marker.
	notDrops := []error{
		nil,
		context.DeadlineExceeded,
		errors.New("unexpected EOF"),
		errors.New("workload log said: connection reset by peer"),
		errors.New("connection refused"),
		errors.New("ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)"),
	}
	for _, err := range notDrops {
		if core.IsConnDrop(err) {
			t.Errorf("IsConnDrop(%v) = true, want false", err)
		}
	}
}

type attestingEnv struct {
	reason  string
	ok      bool
	asked   bool
	gotAddr string
}

func (e *attestingEnv) ControllerURL() string                            { return "" }
func (e *attestingEnv) Versions() core.DetectedVersions                  { return core.DetectedVersions{} }
func (e *attestingEnv) Exec() exec.RemoteExecutor                        { return nil }
func (e *attestingEnv) Handles() core.EnvHandles                         { return core.EnvHandles{} }
func (e *attestingEnv) Collect(context.Context, core.ArtifactSink) error { return nil }
func (e *attestingEnv) AttestPGConnDrop(_ context.Context, addr string) (string, bool) {
	e.asked = true
	e.gotAddr = addr
	return e.reason, e.ok
}

func runFailingBody(t *testing.T, env core.Environment, bodyErr error) *core.Result {
	t.Helper()
	return core.RunOneOnEnv(context.Background(), core.Test{
		Name: "features/conn_drop_probe",
		Run:  func(*core.T) error { return bodyErr },
	}, env, core.RunOptions{Config: core.NewConfig(), Seed: core.NewRunSeed(1, true)})
}

func TestConnDropAttestedBecomesEnvFailed(t *testing.T) {
	env := &attestingEnv{reason: "pod survived, postgres accepts again", ok: true}
	r := runFailingBody(t, env, fmt.Errorf(`exec "INSERT": %w`,
		&pg.TransportError{Err: errors.New("unexpected EOF"), Addr: "127.0.0.1:5001"}))
	if !env.asked {
		t.Fatal("the env was never asked to attest")
	}
	if env.gotAddr != "127.0.0.1:5001" {
		t.Fatalf("attester got addr %q, want the dropped connection's", env.gotAddr)
	}
	if r.Status != core.StatusEnvFailed {
		t.Fatalf("status = %s, want env_failed", r.Status)
	}
	if !strings.Contains(r.ErrorSummary, "unexpected EOF") || !strings.Contains(r.ErrorSummary, env.reason) {
		t.Fatalf("error summary must keep the original error and carry the evidence; got %q", r.ErrorSummary)
	}
}

func TestConnDropUnattestedStaysFailed(t *testing.T) {
	env := &attestingEnv{ok: false}
	r := runFailingBody(t, env, &pg.TransportError{Err: errors.New("unexpected EOF"), Addr: "127.0.0.1:5001"})
	if !env.asked {
		t.Fatal("the env was never asked to attest")
	}
	if r.Status != core.StatusFailed {
		t.Fatalf("status = %s, want failed", r.Status)
	}
}

func TestConnDropWithoutAddrIsNeverAttested(t *testing.T) {
	env := &attestingEnv{reason: "would have attested", ok: true}
	r := runFailingBody(t, env, &pg.TransportError{Err: errors.New("unexpected EOF")})
	if env.asked {
		t.Fatal("the env was asked to attest a drop with no address")
	}
	if r.Status != core.StatusFailed {
		t.Fatalf("status = %s, want failed", r.Status)
	}
}
