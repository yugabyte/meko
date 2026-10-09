package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// A cluster, branch or endpoint create plans a task, and its 202 cannot say how
// that task went. Read as success, a task that then failed was invisible: the
// resource was still absent on the next tick, so the engine planned the same
// create again, and a refusal the controller can only make from inside the task —
// a second read_write endpoint on a branch — became one failed task per tick with
// nothing on the resource (CLOUDGA-37405).
//
// So a create is followed like an update (applyUpdate/priorTask): the task is
// recorded, the resource reports Pending while it runs, and a failure reports the
// controller's own error. A conflict is the one failure classified further,
// because it may or may not clear by itself — see createFailed.

// createMark is the task a create planned.
type createMark struct {
	task openapi_types.UUID
	// gen is the generation the create carried. A create sends no ycp_generation
	// and a cluster echoes none, so once the task succeeds this is the only record
	// that the declared spec landed — see priorCreate.
	gen int64
	// removals is the parent's removal count when the create was issued.
	removals int
	// listedWait is set once Create has seen the task succeed while the resource
	// was still unlisted: the List of that tick ran before the task finished.
	listedWait bool
}

// rejectMark is a create rejected for a conflict, held until something under its
// parent is removed (see Reconsider).
type rejectMark struct {
	parent   string
	removals int
}

// createTask issues a create that plans a task and records the task for the next
// tick to read. The status mapping is mutate's, not mutateTask's: on create a 409
// means the resource already exists or its create is in flight, not that a lock is
// held.
func (b *Backend) createTask(ctx context.Context, r api.DesiredResource,
	call func(context.Context) (*http.Response, error)) error {

	key := reconcile.ResourceKey{Kind: r.Kind, Name: r.Name}
	if gen, err := b.priorCreate(ctx, key); err != nil {
		return err
	} else if gen != nil && b.awaitListing(key) {
		return reconcile.Pending(fmt.Errorf("created; waiting for the controller to list it"))
	}
	b.mu.Lock()
	delete(b.rejected, key)
	b.mu.Unlock()

	resp, err := call(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return reconcile.Rejected(httpErr(resp.StatusCode, body))
	case resp.StatusCode >= 300:
		return httpErr(resp.StatusCode, body)
	}
	id, ok := taskIn(body)
	if !ok {
		return nil
	}
	b.mu.Lock()
	b.creating[key] = createMark{task: id, gen: r.Generation, removals: b.removals[parentPath(r.Name)]}
	b.mu.Unlock()
	return reconcile.Pending(fmt.Errorf("controller task %s is creating it", id))
}

// awaitListing reports whether a create whose task succeeded should wait one tick
// for List to show the resource instead of being issued again. The task can
// finish between a tick's List and its Create; re-issuing then is no no-op for a
// cluster or branch, whose handler plans a duplicate name as a task that fails
// with conflict, and that failure would replace the success Update needs to read.
// One tick only, so a resource deleted since it was created is still re-created.
func (b *Backend) awaitListing(key reconcile.ResourceKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.creating[key]
	if !ok {
		return false
	}
	if m.listedWait {
		delete(b.creating, key)
		return false
	}
	m.listedWait = true
	b.creating[key] = m
	return true
}

// priorCreate is the verdict on the task the last create for key planned: Pending
// while it runs, its error once it failed, and — once it succeeded — the
// generation it carried, which Update reads as applied. Without that a cluster
// created at a generation above 0 (re-declared after a rejection, say) would be
// planned an Update the moment it appeared, and a cluster has no update path. Nil
// with no error when no create is recorded. A succeeded create's mark is kept for
// the caller to retire (see awaitListing, Update).
func (b *Backend) priorCreate(ctx context.Context, key reconcile.ResourceKey) (*int64, error) {
	b.mu.RLock()
	m, ok := b.creating[key]
	b.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	status, taskErr, code, err := b.taskOutcome(ctx, m.task)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read task %s for %s: %w", m.task, key, err)
	case status == taskStatusPending || status == taskStatusRunning:
		return nil, reconcile.Pending(fmt.Errorf("controller task %s is creating it (%s)", m.task, status))
	}
	if status == taskStatusFailed {
		b.mu.Lock()
		delete(b.creating, key)
		b.mu.Unlock()
		return nil, b.createFailed(ctx, key, m, fmt.Errorf("controller task %s failed: %s", m.task, taskErr), code)
	}
	return &m.gen, nil
}

// createFailed classifies a failed create. Anything but a conflict is retried on
// the engine's backoff.
//
// A conflict is held (Rejected) only when nothing under the same parent has gone
// or is going since the create was issued. Otherwise it is likely the swap case:
// one declaration withdraws a branch's read_write endpoint and adds another, the
// engine issues the delete and the create in the same tick, and the create's task
// runs while the old row is still there. That clears by itself once the delete
// lands, so it is retried rather than held. A rejection held here is still
// retired when something under the parent is later removed (Reconsider), so
// withdrawing the conflicting resource in a later declaration is enough.
func (b *Backend) createFailed(ctx context.Context, key reconcile.ResourceKey, m createMark, err error, code string) error {
	if code != taskErrorConflict {
		return err
	}
	parent := parentPath(key.Name)
	b.mu.RLock()
	removed := b.removals[parent] != m.removals
	b.mu.RUnlock()
	if removed || b.deleteInFlight(ctx, parent) {
		return err
	}
	b.mu.Lock()
	b.rejected[key] = rejectMark{parent: parent, removals: b.removals[parent]}
	b.mu.Unlock()
	return reconcile.Rejected(err)
}

// deleteInFlight reports whether a delete this backend issued under parent could
// still free what a create there conflicted with: its task is running, or it
// finished after this tick's List and so is not counted in removals yet. A task
// that could not be read counts as in flight, which errs toward retrying. A
// finished one counts once and is dropped: if its resource is still listed next
// time (a 404 task reads as succeeded), it is not freeing anything.
func (b *Backend) deleteInFlight(ctx context.Context, parent string) bool {
	b.mu.RLock()
	var candidates []reconcile.ResourceKey
	for k := range b.deleting {
		if parentPath(k.Name) == parent {
			candidates = append(candidates, k)
		}
	}
	b.mu.RUnlock()

	inFlight := false
	for _, k := range candidates {
		b.mu.RLock()
		id := b.deleting[k]
		b.mu.RUnlock()
		status, _, _, err := b.taskOutcome(ctx, id)
		switch {
		case err != nil, status == taskStatusPending, status == taskStatusRunning:
			inFlight = true
		case status == taskStatusSucceeded:
			inFlight = true
			fallthrough
		default:
			b.mu.Lock()
			delete(b.deleting, k)
			b.mu.Unlock()
		}
	}
	return inFlight
}

// deleteTask issues a delete that plans a task, with mutate's status mapping, and
// records the task so a conflicting create under the same parent can be told it
// may clear (deleteInFlight).
func (b *Backend) deleteTask(ctx context.Context, key reconcile.ResourceKey,
	call func(context.Context) (*http.Response, error)) error {

	resp, err := call(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return reconcile.Rejected(httpErr(resp.StatusCode, body))
	case resp.StatusCode >= 300:
		return httpErr(resp.StatusCode, body)
	}
	if id, ok := taskIn(body); ok {
		b.mu.Lock()
		b.deleting[key] = id
		b.mu.Unlock()
	}
	return nil
}

// noteRemovals counts, per parent path, the clusters, branches and endpoints
// present in the previous listing and gone from this one (or replaced by a row
// with a new id), and drops the delete marks of the ones that went. Only called
// with a complete listing — a partial one is never indexed — so an unread
// resource is not mistaken for a removed one. It also drops the create mark of a
// listed resource created at generation 0: the engine plans nothing for it, so
// nothing would read the mark again. Caller holds mu.
func (b *Backend) noteRemovals(prev, cur index) {
	for k, m := range b.creating {
		if _, listed := cur.path(k); listed && m.gen == 0 {
			delete(b.creating, k)
		}
	}
	for kind, maps := range map[string][2]map[string]openapi_types.UUID{
		KindCluster:  {prev.cluster, cur.cluster},
		KindBranch:   {prev.branch, cur.branch},
		KindEndpoint: {prev.endpoint, cur.endpoint},
	} {
		for path, id := range maps[0] {
			if now, ok := maps[1][path]; !ok || now != id {
				b.removals[parentPath(path)]++
				delete(b.deleting, reconcile.ResourceKey{Kind: kind, Name: path})
			}
		}
	}
}

// Reconsider implements reconcile.Reconsiderer: the creates rejected for a
// conflict whose parent has since lost a child, so the engine asks again. The
// retry is wasted if what went was not the conflicting resource, and it is
// rejected again; that costs one task.
func (b *Backend) Reconsider() []reconcile.ResourceKey {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []reconcile.ResourceKey
	for k, m := range b.rejected {
		if b.removals[m.parent] != m.removals {
			out = append(out, k)
			delete(b.rejected, k)
		}
	}
	return out
}

// taskIn returns the id of the Task a 2xx body carries, if it carries one.
func taskIn(body []byte) (openapi_types.UUID, bool) {
	var t struct {
		Id openapi_types.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.Id == (openapi_types.UUID{}) {
		return openapi_types.UUID{}, false
	}
	return t.Id, true
}

// path returns the id cur lists for a cluster, branch or endpoint key.
func (idx index) path(k reconcile.ResourceKey) (openapi_types.UUID, bool) {
	var m map[string]openapi_types.UUID
	switch k.Kind {
	case KindCluster:
		m = idx.cluster
	case KindBranch:
		m = idx.branch
	case KindEndpoint:
		m = idx.endpoint
	}
	id, ok := m[k.Name]
	return id, ok
}
