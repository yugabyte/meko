// Package apps is the root of the atomic reuse layer (architecture §4.2,
// implementation §2/§3 M3): App is the uniform lifecycle every workload
// driver and client-pod implementation shares. Deliberately decoupled from
// core (no import of it, mirroring every other apps/* package) so Apps stay
// usable from a test, a task, a nemesis action, or a manual env alike.
package apps

import (
	"context"
	"errors"
	"fmt"
)

// ArtifactSink receives collected blobs (logs, cores, configs). Mirrors
// core.ArtifactSink's method set exactly, so any core.ArtifactSink satisfies
// this by structural typing without apps/ importing core.
type ArtifactSink interface {
	Add(name, kind string, content []byte) error
}

// AppSignal is a lifecycle signal sent to a running App.
type AppSignal int

const (
	SignalStop AppSignal = iota
	SignalPause
	SignalKill
)

func (s AppSignal) String() string {
	switch s {
	case SignalStop:
		return "stop"
	case SignalPause:
		return "pause"
	case SignalKill:
		return "kill"
	default:
		return "unknown"
	}
}

// App is the atomic reuse unit: one deployable/runnable thing behind a
// uniform lifecycle, reused across tests, nemesis, tasks, and manual envs.
// Concrete Apps (client pods, workload drivers) implement this however they
// reach their target — docker over ssh, kubectl, a direct pg connection.
type App interface {
	WaitReady(ctx context.Context) error
	Signal(ctx context.Context, sig AppSignal) error
	Collect(ctx context.Context, into ArtifactSink) error
	Close(ctx context.Context) error
}

// StopWithCollect enforces the report_kill lesson (never lose workload logs
// to a kill): it always runs collect before stop, and always runs stop even
// when collect fails, since losing the ability to stop at all is worse than a
// partial collection. A collect failure is still surfaced in the returned
// error.
func StopWithCollect(
	ctx context.Context,
	into ArtifactSink,
	collect func(context.Context, ArtifactSink) error,
	stop func(context.Context) error,
) error {
	collectErr := collect(ctx, into)
	stopErr := stop(ctx)
	if collectErr != nil {
		// Preserve both: a stop failure must not be masked by a collect failure.
		return errors.Join(fmt.Errorf("collect before stop: %w", collectErr), stopErr)
	}
	return stopErr
}
