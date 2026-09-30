package ampbackend

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// fleetName is the only name a fleet-version resource may carry: one controller,
// one endpoint fleet.
const fleetName = "fleet"

// fleetSpec is a fleet-version resource's desired spec: the version every
// endpoint should run. It can only be reached once the controller deploys it —
// the controller rolls endpoints onto its own pin, never onto a caller's version
// — which is why the kind's parent is the controller.
type fleetSpec struct {
	Version string `json:"version"`
}

// States the fleet row reports.
const (
	fleetStateReady     = "ready"
	fleetStateRolling   = "rolling"
	fleetStateQueued    = "queued"
	fleetStateBehind    = "behind"
	fleetStateUpgrading = "upgrading"
)

func (b *Backend) fleetTarget() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.fleetVersion
}

func (b *Backend) setFleetTarget(v string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fleetVersion = v
}

// fleetActual reports the fleet row, and whether this agent manages the fleet.
//
// Only once a fleet-version has been declared to this process: until then there
// is no row, the engine plans a Create for a declaration, and Create is Update. So
// an environment that declares no fleet never reads the route, and an agent
// restart costs one tick.
//
// Like the controller row it never fails List: an unreachable controller — or
// one too old to serve GET /fleet-version, which is exactly the state mid-upgrade
// — is a not-Ready row in state "upgrading". Preexisting, because the fleet
// exists on the controller whether or not anyone declares it; withdrawing the
// declaration stops managing it and cancels nothing. The row then keeps reporting
// against the last declared version until the agent restarts — reported only,
// never acted on, since nothing plans an update for an undeclared row.
func (b *Backend) fleetActual(ctx context.Context) (reconcile.Actual, bool) {
	target := b.fleetTarget()
	if target == "" {
		return reconcile.Actual{}, false
	}
	a := reconcile.Actual{
		Key:         reconcile.ResourceKey{Kind: KindFleetVersion, Name: fleetName},
		Parent:      reconcile.ResourceKey{Kind: KindController, Name: controllerName},
		State:       fleetStateUpgrading,
		Preexisting: true,
		// No create and no echo, as for the controller: the generation diff is the
		// only lever, and updateFleet decides whether there is anything to do.
		ObservedGen: reconcile.GenerationNever,
	}
	fv, err := b.readFleetVersion(ctx)
	if err != nil {
		a.Attrs = attrPut(nil, "target_version", target)
		return a, true
	}
	switch {
	case fv.Roll != nil && fv.Roll.Status == ampclient.TaskStatusRunning:
		a.State = fleetStateRolling
		a.Progress = b.rollProgress(ctx, fv.Roll)
	case fv.Roll != nil:
		a.State = fleetStateQueued
	case deref(fv.Version) == target:
		a.State, a.Ready = fleetStateReady, true
	default:
		a.State = fleetStateBehind
	}
	a.Attrs = attrPut(attrPut(attrPut(nil, "version", deref(fv.Version)),
		"endpoint_version", deref(fv.TargetVersion)), "target_version", target)
	if fv.Roll != nil {
		a.Attrs = attrPut(attrPut(a.Attrs, "roll_task_id", fv.Roll.Id.String()), "roll_progress", a.Progress)
	}
	return a, true
}

func (b *Backend) readFleetVersion(ctx context.Context) (ampclient.FleetVersion, error) {
	var fv ampclient.FleetVersion
	err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.GetFleetVersion(c)
	}, &fv)
	return fv, err
}

// rollProgress is a running roll's done/total, from its children. It feeds
// Actual.Progress, which is what keeps a long roll from reading as stuck. A read
// that fails repeats the last value read for this roll: any change restarts the
// engine's stuck timers, so answering "" would read a wedged roll whose reads fail
// now and then as one that keeps moving.
func (b *Backend) rollProgress(ctx context.Context, roll *ampclient.Task) string {
	var subs ampclient.SubtaskList
	if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.ListTaskSubtasks(c, roll.Id)
	}, &subs); err != nil {
		b.mu.RLock()
		defer b.mu.RUnlock()
		if b.lastProgressRoll == roll.Id {
			return b.lastProgress
		}
		return ""
	}
	done := 0
	for _, s := range subs.Subtasks {
		if s.Status == ampclient.TaskStatusSucceeded || s.Status == ampclient.TaskStatusFailed {
			done++
		}
	}
	p := fmt.Sprintf("%d/%d", done, subs.Count)
	b.mu.Lock()
	b.lastProgressRoll, b.lastProgress = roll.Id, p
	b.mu.Unlock()
	return p
}

// updateFleet converges the fleet onto spec.version.
//
// Returning nil is what writes the engine's last-applied cache, so it does so
// only when the controller's fleet version already equals the spec; everything in
// flight is Pending. A spec naming a version the controller does not deploy yet
// is Pending too, not rejected: the controller upgrade that moves the pin may be
// issued in this same tick, and the only record of its target is set by that
// Update, which may not have run yet.
func (b *Backend) updateFleet(ctx context.Context, r api.DesiredResource) error {
	if r.Name != fleetName {
		return reconcile.Permanent(fmt.Errorf(
			"fleet-version %q: the fleet is a singleton and must be declared as %q", r.Name, fleetName))
	}
	var spec fleetSpec
	if err := unmarshalSpec(r, &spec); err != nil {
		return err
	}
	version := strings.TrimSpace(spec.Version)
	if version == "" {
		return reconcile.Rejected(fmt.Errorf("fleet-version %q: spec.version is required", r.Name))
	}
	b.setFleetTarget(version)

	fv, err := b.readFleetVersion(ctx)
	if err != nil {
		return fmt.Errorf("read fleet version: %w", err)
	}
	key := reconcile.ResourceKey{Kind: KindFleetVersion, Name: fleetName}
	switch pin := deref(fv.TargetVersion); {
	case deref(fv.Version) == version:
		b.clearInflight(key)
		return nil
	case fv.Roll != nil:
		return reconcile.Pending(fmt.Errorf("endpoint roll %s is %s", fv.Roll.Id, fv.Roll.Status))
	case pin != version:
		return reconcile.Pending(fmt.Errorf(
			"the controller deploys endpoints at %q, not %q; waiting for the controller to move", pin, version))
	}
	return b.applyUpdate(ctx, key, r.Generation, func(c context.Context, _ int64) (*http.Response, error) {
		return b.api.UpgradeEndpoints(c, ampclient.UpgradeEndpointsRequest{})
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
