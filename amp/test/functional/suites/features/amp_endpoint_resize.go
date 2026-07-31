// This file carries the functional tests for in-place vertical resize of a
// compute endpoint (POST …/endpoints/{id}/resize). They drive the same
// stood-up-control-plane flow the other amp feature tests use and assert what
// the product guarantees: the new size is applied and durable, an omitted
// dimension is preserved, a `completed` in-place resize keeps the SAME pod with
// no container restart and no dropped connection, and concurrent resizes of one
// endpoint are first-writer-wins (one 202, the rest 409, lock released on
// terminal).
//
// A deferred (`pending`), unsatisfiable (`infeasible`) or runtime-unsupported
// (`unsupported`) in-place resize is a SUCCESSFUL terminal outcome in the
// product — the size is persisted and converges on the next redeploy — so these
// tests assert persistence in those cases rather than failing.
package features

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// resizeCPUA/resizeMemoryA and resizeCPUB/resizeMemoryB are the two sizes the
	// tests move between. Both differ from the controller's create-time defaults
	// ("2" / "4Gi"), which the API substitutes for an unset dimension — resizing
	// to a default would make "persisted" indistinguishable from "defaulted".
	resizeCPUA    = "3"
	resizeMemoryA = "5Gi"
	resizeCPUB    = "4"
	resizeMemoryB = "6Gi"
	// resizeContenders is how many concurrent resizes the lock test fires at one
	// endpoint. The lock admits one; the rest must be rejected with 409.
	resizeContenders = 6
	// resizeHeldTable is the table the in-place test writes before the resize and
	// re-reads afterwards over the connection its transaction pinned.
	resizeHeldTable = "amp_resize_held"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Resize an endpoint's cpu and then its memory, asserting each lands in the endpoint's persisted reservation and that the dimension left out of a request keeps its previous value.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointResize,
	})

	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize_in_place",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Resize a running endpoint and assert a completed in-place resize keeps the same compute pod with no container restart while an already-open SQL connection keeps serving.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointResizeInPlace,
	})

	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize_lock",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Fire concurrent resizes at one endpoint and assert first-writer-wins: exactly one 202, the rest 409, the endpoint lands at the winner's size, and the lock releases afterwards.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointResizeLock,
	})

	core.Register(core.Test{
		Name:        "features/amp_endpoint_resize_read_only",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Resize a read_only-role endpoint and assert it takes the new reservation the same way a read_write endpoint does.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointResizeReadOnly,
	})
}

// resizePodIdentity is the compute pod a resize must not disturb: which pod is
// serving the endpoint, and how many times the containers in it have restarted.
type resizePodIdentity struct {
	name     string
	restarts int
}

func runAmpEndpointResize(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize", a.endpointResize}}
	})
}

func runAmpEndpointResizeInPlace(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize_in_place", a.endpointResizeInPlace}}
	})
}

func runAmpEndpointResizeLock(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize_lock", a.endpointResizeLock}}
	})
}

func runAmpEndpointResizeReadOnly(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_resize_read_only", a.endpointResizeReadOnly}}
	})
}

// endpointResize asserts a resize lands in the endpoint's persisted reservation
// and that the dimension a request leaves out keeps its previous value. It
// resizes cpu alone, then memory alone, checking the other dimension each time.
func (a *ampFeatureRun) endpointResize(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	before, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}

	// cpu only: memory must keep the value it had.
	cpuOnly, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{CPU: resizeCPUA})
	if err != nil {
		return err
	}
	a.f.logf("resized cpu to %s: in-place outcome %q", resizeCPUA, cpuOnly)
	if err := a.assertResources(ctx, cl, ep.ID, resizeCPUA, before.Memory); err != nil {
		return err
	}

	// memory only: cpu must keep the value the previous resize gave it.
	memOnly, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{Memory: resizeMemoryA})
	if err != nil {
		return err
	}
	a.f.logf("resized memory to %s: in-place outcome %q", resizeMemoryA, memOnly)
	return a.assertResources(ctx, cl, ep.ID, resizeCPUA, resizeMemoryA)
}

// endpointResizeInPlace asserts the "in place, not a restart" guarantee: on a
// completed in-place resize the endpoint is served by the SAME pod with an
// unchanged container-restart total, and a connection opened before the resize
// still serves afterwards. On a deferred/infeasible/unsupported outcome the pod
// checks do not apply (the kubelet has not adopted the size yet) — the size is
// still persisted, which is the documented behaviour, so that is what is asserted.
func (a *ampFeatureRun) endpointResizeInPlace(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	before, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}

	client, err := a.endpointPGClient(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer client.Close()
	if _, err := client.Exec(ctx, "CREATE TABLE "+resizeHeldTable+" (id int primary key)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := client.Exec(ctx, "INSERT INTO "+resizeHeldTable+" (id) VALUES (1)"); err != nil {
		return fmt.Errorf("seed row: %w", err)
	}

	// A transaction pins ONE pooled connection for its whole life, so querying in
	// it after the resize is the testable form of "an in-place resize drops no
	// connection". Reading in it first proves the connection was live going in.
	held, err := client.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the transaction held across the resize: %w", err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if _, err := held.Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("the transaction held across the resize is not usable before it: %w", err)
	}

	probe := &kubeComputeRoller{kube: a.f.ampKube(), timeout: restartReadyTimeout}
	podBefore, err := endpointPodIdentity(ctx, probe, ep.ID)
	if err != nil {
		a.f.t.Skip("cannot read the endpoint's compute pod, so an in-place resize cannot be distinguished from a restart: " + err.Error())
		return nil
	}

	outcome, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{CPU: resizeCPUB, Memory: resizeMemoryB})
	if err != nil {
		return err
	}
	a.f.logf("resized to cpu=%s memory=%s: in-place outcome %q (pod %s, %d container restarts before)",
		resizeCPUB, resizeMemoryB, outcome, podBefore.name, podBefore.restarts)

	// The size is persisted regardless of the in-place outcome.
	if err := a.assertResources(ctx, cl, ep.ID, resizeCPUB, resizeMemoryB); err != nil {
		return err
	}

	if outcome == controller.ResizeCompleted {
		podAfter, err := endpointPodIdentity(ctx, probe, ep.ID)
		if err != nil {
			return fmt.Errorf("read endpoint compute pod after resize: %w", err)
		}
		if podAfter.name != podBefore.name {
			return fmt.Errorf("in-place resize reported %q but the endpoint is now served by a different pod (%s before, %s after) — a completed in-place resize must keep the running pod",
				outcome, podBefore.name, podAfter.name)
		}
		if podAfter.restarts != podBefore.restarts {
			return fmt.Errorf("in-place resize reported %q but pod %s went from %d to %d container restarts — a completed in-place resize must not restart a container",
				outcome, podAfter.name, podBefore.restarts, podAfter.restarts)
		}
	} else {
		a.f.logf("in-place outcome %q (not %q): the pod-identity assertions do not apply; the size %s/%s stayed persisted and converges on the next redeploy",
			outcome, controller.ResizeCompleted, resizeCPUB, resizeMemoryB)
	}

	// The connection pinned before the resize must still serve, and still see its
	// own data.
	var rows int
	if err := held.QueryRow(ctx, "SELECT count(*) FROM "+resizeHeldTable).Scan(&rows); err != nil {
		return fmt.Errorf("the connection pinned before the resize (in-place outcome %q, reservation %s/%s → %s/%s) no longer serves queries, want it unaffected: %w",
			outcome, before.CPU, before.Memory, resizeCPUB, resizeMemoryB, err)
	}
	if rows != 1 {
		return fmt.Errorf("across the resize the pinned connection sees %d rows in %s, want 1", rows, resizeHeldTable)
	}
	return nil
}

// endpointPGClient opens a pooled SQL client to an endpoint, concretely typed (not
// the sqlConn seam) so the in-place test can pin one connection across the resize.
func (a *ampFeatureRun) endpointPGClient(ctx context.Context, ep controller.Endpoint) (*pg.Client, error) {
	if a.f.ampEnv != nil {
		return a.f.ampEnv.ConnectEndpoint(ctx, ep)
	}
	ci, err := a.f.endpointConnInfo(ep)
	if err != nil {
		return nil, err
	}
	return connectPG(ctx, ci)
}

// endpointResizeLock asserts first-writer-wins. It fires resizeContenders
// concurrent resizes, each at a distinct cpu, and asserts exactly one is accepted
// while every other is rejected with 409, the endpoint lands at the winner's cpu,
// and a fresh resize is accepted once the winner reached terminal (the partial
// unique index task_one_active_resize_per_endpoint drops the terminal task out of
// its predicate, releasing the lock).
func (a *ampFeatureRun) endpointResizeLock(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	if _, err := a.endpointResources(ctx, cl, ep.ID); err != nil {
		return err
	}

	type attempt struct {
		cpu    string
		task   controller.Task
		status int // HTTP status of a rejection; 0 when the resize was accepted
		err    error
	}
	attempts := make([]attempt, resizeContenders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range attempts {
		// Distinct cpu per contender, so the endpoint's final size identifies the
		// winner rather than being satisfied by any of them.
		attempts[i].cpu = strconv.Itoa(3 + i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			task, _, err := cl.ResizeEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID,
				controller.ResizeEndpointRequest{Resources: controller.EndpointResources{CPU: attempts[i].cpu}})
			attempts[i].task, attempts[i].err = task, err
			attempts[i].status = apiErrorStatus(err)
		}(i)
	}
	close(start)
	wg.Wait()

	var winners []attempt
	for _, at := range attempts {
		switch {
		case at.err == nil:
			winners = append(winners, at)
		case at.status == http.StatusConflict:
		default:
			return fmt.Errorf("a concurrent resize (cpu=%s) failed with an unexpected error, want either acceptance or 409 conflict: %w", at.cpu, at.err)
		}
	}
	if len(winners) != 1 {
		accepted := make([]string, 0, len(winners))
		for _, w := range winners {
			accepted = append(accepted, w.cpu)
		}
		return fmt.Errorf("%d of %d concurrent resizes of endpoint %s were accepted (cpu %v), want exactly 1 with the rest rejected 409 — at most one non-terminal resize_endpoint task per endpoint may exist",
			len(winners), len(attempts), ep.ID, accepted)
	}
	winner := winners[0]
	a.f.logf("first-writer-wins: cpu=%s accepted, %d rejected with 409", winner.cpu, len(attempts)-1)

	outcome, err := a.resizeOutcome(cl.WaitTask(ctx, winner.task.ID))
	if err != nil {
		return err
	}
	a.f.logf("winning resize settled with in-place outcome %q", outcome)
	if err := a.assertResourcesCPU(ctx, cl, ep.ID, winner.cpu); err != nil {
		return err
	}

	// The lock self-releases on terminal: a fresh resize must be accepted now.
	if _, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{CPU: resizeCPUB}); err != nil {
		return fmt.Errorf("a resize issued after the winning resize reached terminal was not accepted, want the first-writer-wins lock released: %w", err)
	}
	return a.assertResourcesCPU(ctx, cl, ep.ID, resizeCPUB)
}

// endpointResizeReadOnly asserts a read_only-role endpoint takes a resize the
// same way a read_write one does. The role is not yet plumbed into a compute mode
// (see features/amp_read_replica, which is skipped for exactly that), so the test
// skips when such an endpoint does not come up rather than reporting a resize
// failure for an unrelated reason.
func (a *ampFeatureRun) endpointResizeReadOnly(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	if _, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID); err != nil {
		return fmt.Errorf("create read_write endpoint: %w", err)
	}
	ep, err := createRoleEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID, roleReadOnly)
	if err != nil {
		a.f.t.Skip("no read_only endpoint came up to resize: " + err.Error())
		return nil
	}
	if ep.Role != roleReadOnly {
		return fmt.Errorf("the endpoint created for role %s reports role %q, want %s", roleReadOnly, ep.Role, roleReadOnly)
	}
	before, err := a.endpointResources(ctx, cl, ep.ID)
	if err != nil {
		return err
	}

	outcome, err := a.resize(ctx, cl, ep.ID, controller.EndpointResources{CPU: resizeCPUA})
	if err != nil {
		return err
	}
	a.f.logf("resized read_only endpoint cpu %s → %s: in-place outcome %q", before.CPU, resizeCPUA, outcome)
	return a.assertResources(ctx, cl, ep.ID, resizeCPUA, before.Memory)
}

// resize issues one resize, waits for its task, and returns the in-place outcome
// it reported.
func (a *ampFeatureRun) resize(ctx context.Context, cl *controller.Client, endpointID string, want controller.EndpointResources) (string, error) {
	task, err := cl.ResizeEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, endpointID,
		controller.ResizeEndpointRequest{Resources: want})
	return a.resizeOutcome(task, err)
}

// resizeOutcome validates a settled resize task and returns the in-place outcome
// it recorded, which must be one of the four the API documents. It skips the test
// when the task failed because the runtime cannot apply an in-place pod change at
// all — nothing about the product is under test on such a cluster.
func (a *ampFeatureRun) resizeOutcome(task controller.Task, err error) (string, error) {
	if err != nil {
		if resizeInPlaceUnsupportedByRuntime(err) {
			a.f.t.Skip("the runtime cannot apply an in-place pod resize (the deployer's own pod-level resize failed; the pods/resize subresource needs Kubernetes 1.33+): " + err.Error())
		}
		return "", err
	}
	if task.Status != controller.TaskSucceeded {
		return "", fmt.Errorf("resize task %s settled at status %q, want %q", task.ID, task.Status, controller.TaskSucceeded)
	}
	outcome, message := task.InPlaceResize()
	switch outcome {
	case controller.ResizeCompleted, controller.ResizePending, controller.ResizeInfeasible, controller.ResizeUnsupported:
		return outcome, nil
	}
	return "", fmt.Errorf("resize task %s recorded in-place outcome %q (%q), want one of %s/%s/%s/%s",
		task.ID, outcome, message,
		controller.ResizeCompleted, controller.ResizePending, controller.ResizeInfeasible, controller.ResizeUnsupported)
}

// endpointResources reads the endpoint's current reservation, skipping the test on
// a controller that predates in-place resize. The endpoint response carries
// `resources` only on builds that have the feature, and the framework has no amp
// build marker to version-gate on, so the product's own response shape is the gate.
func (a *ampFeatureRun) endpointResources(ctx context.Context, cl *controller.Client, endpointID string) (controller.EndpointResources, error) {
	ep, err := cl.GetEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, endpointID)
	if err != nil {
		return controller.EndpointResources{}, fmt.Errorf("get endpoint %s: %w", endpointID, err)
	}
	if ep.Resources == nil {
		a.f.t.Skip("this controller build does not report an endpoint's resources, so it predates in-place endpoint resize")
		return controller.EndpointResources{}, nil
	}
	return *ep.Resources, nil
}

// assertResources re-reads the endpoint and asserts its persisted reservation is
// exactly wantCPU/wantMemory. The reported values come from the endpoint's config
// jsonb, so this is also the assertion that a redeploy would keep the new size.
func (a *ampFeatureRun) assertResources(ctx context.Context, cl *controller.Client, endpointID, wantCPU, wantMemory string) error {
	got, err := a.endpointResources(ctx, cl, endpointID)
	if err != nil {
		return err
	}
	if got.CPU != wantCPU || got.Memory != wantMemory {
		return fmt.Errorf("after the resize, endpoint %s reports cpu=%q memory=%q, want cpu=%q memory=%q (the reported reservation is read back from the endpoint's persisted config, so a redeploy would keep the wrong size)",
			endpointID, got.CPU, got.Memory, wantCPU, wantMemory)
	}
	return nil
}

// assertResourcesCPU asserts only the cpu dimension, for the lock test where the
// contenders differ in cpu alone.
func (a *ampFeatureRun) assertResourcesCPU(ctx context.Context, cl *controller.Client, endpointID, wantCPU string) error {
	got, err := a.endpointResources(ctx, cl, endpointID)
	if err != nil {
		return err
	}
	if got.CPU != wantCPU {
		return fmt.Errorf("endpoint %s reports cpu=%q, want %q", endpointID, got.CPU, wantCPU)
	}
	return nil
}

// endpointPodIdentity reads which pod serves the endpoint and how many restarts
// its containers have accumulated in total. Summing every container's count (not
// indexing one) keeps it correct however many containers the endpoint chart
// renders, and zero change in the total is what "no restart" means.
func endpointPodIdentity(ctx context.Context, r *kubeComputeRoller, endpointID string) (resizePodIdentity, error) {
	name, err := r.podName(ctx, endpointID)
	if err != nil {
		return resizePodIdentity{}, err
	}
	raw, err := r.kube.Get(ctx, "pod", name, "{.status.containerStatuses[*].restartCount}")
	if err != nil {
		return resizePodIdentity{}, fmt.Errorf("read restart counts of pod %s: %w", name, err)
	}
	total := 0
	for _, f := range strings.Fields(raw) {
		n, err := strconv.Atoi(f)
		if err != nil {
			return resizePodIdentity{}, fmt.Errorf("parse restart count %q of pod %s: %w", f, name, err)
		}
		total += n
	}
	return resizePodIdentity{name: name, restarts: total}, nil
}

// resizeInPlaceUnsupportedByRuntime reports a resize task that failed on the
// deployer's own pod-level resize ("resize pod <pod> in place: …") rather than on
// anything the product got wrong — what a cluster without the pods/resize
// subresource (Kubernetes < 1.33) produces. The DB write is ordered after the
// deployer, so nothing was persisted either.
func resizeInPlaceUnsupportedByRuntime(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "resize pod") && strings.Contains(s, "in place")
}

// apiErrorStatus returns the HTTP status a controller API error carries, or 0 for
// a non-API error.
func apiErrorStatus(err error) int {
	var apiErr *controller.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}
