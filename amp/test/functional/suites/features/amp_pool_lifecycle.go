// Storage pools gained rename and delete
// (https://github.com/yugabyte/meko/pull/1108). A pool is
// tombstoned rather than removed, so the contract this test holds is about what
// a tombstone frees and what it keeps: the name is free at once, the id is not;
// only a LIVE cluster blocks the delete; and a cluster create racing the delete
// can never leave a live cluster under a pool no reader can see.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// poolRaceDelays spreads the delete across the create's window: a synchronous
// handler pre-check, the task claiming, and the EnsureCluster INSERT that carries
// the interlock. Zero is included on purpose — unlike the branch-drop race, both
// sides here are cheap and the interlock is a row write inside one transaction,
// so firing back to back is the tightest aim at it.
var poolRaceDelays = []time.Duration{0, 300 * time.Millisecond, 1500 * time.Millisecond}

const (
	poolConcurrentDeletes = 6
	poolDuplicateDeleters = 3
)

// poolRaceJoinBudget bounds waiting out the raced create task.
const poolRaceJoinBudget = 3 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "features/amp_pool_lifecycle",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Storage pool rename and delete: a rename is visible on get and list and frees its old name, a name held by another live pool is refused 409, delete is refused 409 while the pool holds a live cluster and succeeds once that cluster is deleted, a deleted pool is 404 on get, gone from list, its name reusable and its id not; then race a cluster create against the pool delete and assert no live cluster ever sits under a tombstoned pool; finally delete several pools at once, three callers on one of them, and assert no delete answers 500 when its transaction loses a serialization race (meko#1849) and every pool ends deleted.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     20 * time.Minute,
		Run:         runAmpPoolLifecycle,
	})
}

func runAmpPoolLifecycle(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		p := &poolLifecycle{a: a}
		return []phase{
			{"pool_rename", p.rename},
			{"pool_delete_gate", p.deleteGate},
			{"pool_create_delete_race", p.createDeleteRace},
			{"pool_concurrent_deletes", p.concurrentDeletes},
		}
	})
}

type poolLifecycle struct {
	a      *ampFeatureRun
	cl     *controller.Client
	userID string
}

// newPool creates a pool owned by this test's user and registers its delete.
func (p *poolLifecycle) newPool(ctx context.Context, name string) (controller.StoragePool, error) {
	if p.userID == "" {
		user, err := p.cl.CreateUser(ctx, controller.CreateUserRequest{IsAdmin: true})
		if err != nil {
			return controller.StoragePool{}, fmt.Errorf("create user: %w", err)
		}
		p.userID = user.ID
	}
	pool, err := p.cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{Name: name, OwnerUserID: p.userID})
	if err != nil {
		return controller.StoragePool{}, fmt.Errorf("create pool %q: %w", name, err)
	}
	p.a.f.register("delete pool "+name, func(ctx context.Context) error {
		err := p.cl.DeleteStoragePool(ctx, pool.ID)
		if apiErrorStatus(err) == http.StatusNotFound {
			return nil
		}
		return err
	})
	return pool, nil
}

// registerClusterDelete backstops the inline deletes: a cluster left live by a
// failed assertion would otherwise make its pool undeletable at teardown.
func (p *poolLifecycle) registerClusterDelete(poolID, clusterID string) {
	p.a.f.register("delete cluster "+clusterID, func(ctx context.Context) error {
		_, err := p.cl.DeleteClusterAndWait(ctx, poolID, clusterID)
		if apiErrorStatus(err) == http.StatusNotFound {
			return nil
		}
		return err
	})
}

func (p *poolLifecycle) connect(ctx context.Context) error {
	if p.cl != nil {
		return nil
	}
	cl, err := p.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	p.cl = cl
	return nil
}

func (p *poolLifecycle) rename(ctx context.Context) error {
	if err := p.connect(ctx); err != nil {
		return err
	}
	base := "pool-life-" + p.a.f.suffix()
	first, err := p.newPool(ctx, base+"-a")
	if err != nil {
		return err
	}
	renamed, err := p.cl.UpdateStoragePool(ctx, first.ID, controller.UpdateStoragePoolRequest{Name: base + "-a2"})
	if apiErrorStatus(err) == http.StatusMethodNotAllowed {
		p.a.f.t.Skip("this controller has no storage pool rename/delete (PATCH answered 405): " + err.Error())
		return nil
	}
	if err != nil {
		return fmt.Errorf("rename pool %s: %w", first.ID, err)
	}
	if renamed.ID != first.ID || renamed.Name != base+"-a2" {
		return fmt.Errorf("rename returned pool %s named %q, want %s named %q", renamed.ID, renamed.Name, first.ID, base+"-a2")
	}
	got, err := p.cl.GetStoragePool(ctx, first.ID)
	if err != nil {
		return fmt.Errorf("get renamed pool: %w", err)
	}
	if got.Name != base+"-a2" {
		return fmt.Errorf("getStoragePool reports name %q after the rename, want %q", got.Name, base+"-a2")
	}
	if name, ok := poolNameInList(ctx, p.cl, first.ID); !ok || name != base+"-a2" {
		return fmt.Errorf("listStoragePools reports pool %s as %q (listed=%v) after the rename, want %q", first.ID, name, ok, base+"-a2")
	}

	// The old name is free the moment the rename lands.
	second, err := p.newPool(ctx, base+"-a")
	if err != nil {
		return fmt.Errorf("the name %q was released by the rename but a new pool cannot take it: %w", base+"-a", err)
	}
	// And a name another live pool holds is refused, leaving the caller's pool as it was.
	_, err = p.cl.UpdateStoragePool(ctx, second.ID, controller.UpdateStoragePoolRequest{Name: base + "-a2"})
	if status := apiErrorStatus(err); status != http.StatusConflict {
		return fmt.Errorf("renaming pool %s onto the name live pool %s holds returned HTTP %d (%v), want 409", second.ID, first.ID, status, err)
	}
	if got, err := p.cl.GetStoragePool(ctx, second.ID); err != nil {
		return err
	} else if got.Name != base+"-a" {
		return fmt.Errorf("a refused rename changed pool %s's name to %q, want it left at %q", second.ID, got.Name, base+"-a")
	}
	if _, err := p.cl.UpdateStoragePool(ctx, second.ID, controller.UpdateStoragePoolRequest{Name: ""}); apiErrorStatus(err) != http.StatusBadRequest {
		return fmt.Errorf("renaming a pool to an empty name returned %v, want 400", err)
	}
	return nil
}

// deleteGate: a live cluster blocks the delete, a tombstoned one does not, and a
// tombstoned pool frees its name but never its id.
func (p *poolLifecycle) deleteGate(ctx context.Context) error {
	if err := p.connect(ctx); err != nil {
		return err
	}
	name := "pool-gate-" + p.a.f.suffix()
	pool, err := p.newPool(ctx, name)
	if err != nil {
		return err
	}
	if _, err := p.cl.CreateClusterAndWait(ctx, pool.ID, controller.CreateClusterRequest{Name: name, PgVersion: p.a.f.cfg.PgVersion}); err != nil {
		return fmt.Errorf("create cluster on %s: %w", pool.ID, err)
	}
	proj, err := findClusterCtl(ctx, p.cl, pool.ID, name)
	if err != nil {
		return err
	}
	p.registerClusterDelete(pool.ID, proj.ID)
	if status := apiErrorStatus(p.cl.DeleteStoragePool(ctx, pool.ID)); status != http.StatusConflict {
		return fmt.Errorf("deleting pool %s while it holds live cluster %s returned HTTP %d, want 409", pool.ID, proj.ID, status)
	}
	if _, err := p.cl.GetCluster(ctx, pool.ID, proj.ID); err != nil {
		return fmt.Errorf("the refused delete must leave the pool and its cluster untouched, but the cluster read back failed: %w", err)
	}

	if _, err := p.cl.DeleteClusterAndWait(ctx, pool.ID, proj.ID); err != nil {
		return fmt.Errorf("delete cluster %s: %w", proj.ID, err)
	}
	if err := p.cl.DeleteStoragePool(ctx, pool.ID); err != nil {
		return fmt.Errorf("deleting pool %s after its only cluster was deleted (the row is a tombstone, not a live cluster): %w", pool.ID, err)
	}
	if _, err := p.cl.GetStoragePool(ctx, pool.ID); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("getStoragePool on the deleted pool %s returned %v, want 404", pool.ID, err)
	}
	if poolListed(ctx, p.cl, pool.ID) {
		return fmt.Errorf("deleted pool %s is still in listStoragePools", pool.ID)
	}
	if _, err := p.cl.ListClusters(ctx, pool.ID, 10, 0); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("listing clusters of the deleted pool %s returned %v, want 404: a tombstoned pool must be gone for every reader", pool.ID, err)
	}
	if err := p.cl.DeleteStoragePool(ctx, pool.ID); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("deleting the already-deleted pool %s returned %v, want 404", pool.ID, err)
	}
	if _, err := p.cl.UpdateStoragePool(ctx, pool.ID, controller.UpdateStoragePoolRequest{Name: name + "-x"}); apiErrorStatus(err) != http.StatusNotFound {
		return fmt.Errorf("renaming the deleted pool %s returned %v, want 404", pool.ID, err)
	}

	// The name is free at once; the id stays taken by the tombstone.
	again, err := p.newPool(ctx, name)
	if err != nil {
		return fmt.Errorf("the deleted pool's name %q is not reusable: %w", name, err)
	}
	if again.ID == pool.ID {
		return fmt.Errorf("re-creating name %q handed back the deleted pool's id %s: the tombstone was resurrected instead of a new pool created", name, pool.ID)
	}
	_, err = p.cl.CreateStoragePool(ctx, controller.CreateStoragePoolRequest{ID: pool.ID, Name: name + "-byid", OwnerUserID: p.userID})
	if status := apiErrorStatus(err); status != http.StatusConflict {
		if err == nil {
			p.a.f.register("delete resurrected pool", func(ctx context.Context) error { return p.cl.DeleteStoragePool(ctx, pool.ID) })
		}
		return fmt.Errorf("creating a pool with the deleted pool's id %s returned HTTP %d (%v), want 409: the tombstone keeps the primary key", pool.ID, status, err)
	}
	return nil
}

// createDeleteRace fires a cluster create and the pool delete together and holds
// the interlock: either the delete is refused (409) and the cluster lives, or the
// delete wins and the create fails. Never both committing.
func (p *poolLifecycle) createDeleteRace(ctx context.Context) error {
	if err := p.connect(ctx); err != nil {
		return err
	}
	for i, delay := range poolRaceDelays {
		name := fmt.Sprintf("pool-race-%s-%d", p.a.f.suffix(), i)
		pool, err := p.newPool(ctx, name)
		if err != nil {
			return err
		}
		task, _, createErr := p.cl.CreateCluster(ctx, pool.ID, controller.CreateClusterRequest{Name: name, PgVersion: p.a.f.cfg.PgVersion})
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		delErr := p.cl.DeleteStoragePool(ctx, pool.ID)
		createStatus := "not-planned"
		if createErr == nil {
			jctx, cancel := context.WithTimeout(ctx, poolRaceJoinBudget)
			done, err := p.cl.WaitTask(jctx, task.ID)
			cancel()
			createStatus = done.Status
			if createStatus == "" {
				return fmt.Errorf("round %d: the raced create_cluster task %s did not settle within %s: %v", i, task.ID, poolRaceJoinBudget, err)
			}
		}
		verdict, err := poolRaceVerdict(apiErrorStatus(delErr), delErr == nil, createStatus)
		p.a.f.logf("pool race round %d (delete fired %s after the create): delete=%v create=%s -> %s", i, delay, delErr, createStatus, verdict)
		if err != nil {
			return fmt.Errorf("round %d (delete fired %s after the create): %w", i, delay, err)
		}
		if err := p.raceInvariant(ctx, pool.ID, name, delErr == nil); err != nil {
			return fmt.Errorf("round %d: %w", i, err)
		}
	}
	return nil
}

// poolRaceVerdict classifies one round from the delete's answer and the create
// task's terminal status. The forbidden pair is the delete succeeding while the
// create also succeeded.
func poolRaceVerdict(deleteStatus int, deleted bool, createStatus string) (string, error) {
	switch {
	case deleted && createStatus == controller.TaskSucceeded:
		return "", fmt.Errorf("the pool delete returned 204 AND the create_cluster task succeeded: a live cluster now sits under a tombstoned pool, invisible to every dropped_at IS NULL reader and unreachable through the API (the EnsureCluster <-> pool-delete interlock did not serialize them)")
	case deleted:
		return "delete won, create " + createStatus, nil
	case deleteStatus == http.StatusConflict:
		return "create won, delete refused 409", nil
	case deleteStatus == http.StatusNotFound:
		return "", fmt.Errorf("the pool delete returned 404 for a pool created moments earlier")
	default:
		return "", fmt.Errorf("the pool delete failed with HTTP %d, want 204 or 409", deleteStatus)
	}
}

// raceInvariant reads the state the round left behind. A deleted pool must be
// gone with no cluster reachable under it; a surviving pool must hold its cluster
// and is then cleaned up.
func (p *poolLifecycle) raceInvariant(ctx context.Context, poolID, name string, deleted bool) error {
	if deleted {
		if _, err := p.cl.GetStoragePool(ctx, poolID); apiErrorStatus(err) != http.StatusNotFound {
			return fmt.Errorf("pool %s answered the delete with 204 but getStoragePool returns %v, want 404", poolID, err)
		}
		if _, err := p.cl.ListClusters(ctx, poolID, 10, 0); apiErrorStatus(err) != http.StatusNotFound {
			return fmt.Errorf("clusters are still listable under the deleted pool %s (%v)", poolID, err)
		}
		return nil
	}
	proj, err := findClusterCtl(ctx, p.cl, poolID, name)
	if err != nil {
		return fmt.Errorf("the delete was refused 409 so the create must have landed, but: %w", err)
	}
	p.registerClusterDelete(poolID, proj.ID)
	if _, err := p.cl.DeleteClusterAndWait(ctx, poolID, proj.ID); err != nil {
		return fmt.Errorf("delete the surviving cluster %s: %w", proj.ID, err)
	}
	if err := p.cl.DeleteStoragePool(ctx, poolID); err != nil {
		return fmt.Errorf("delete pool %s once its cluster is gone: %w", poolID, err)
	}
	return nil
}

func poolListed(ctx context.Context, cl *controller.Client, poolID string) bool {
	_, ok := poolNameInList(ctx, cl, poolID)
	return ok
}

// poolNameInList pages listStoragePools for the pool and returns its name.
func poolNameInList(ctx context.Context, cl *controller.Client, poolID string) (string, bool) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListStoragePools(ctx, page, offset)
		if err != nil {
			return "", false
		}
		for _, sp := range list.StoragePools {
			if sp.ID == poolID {
				return sp.Name, true
			}
		}
		if list.Count < page || offset+len(list.StoragePools) >= list.Total {
			return "", false
		}
	}
}

// concurrentDeletes lets pool deletes conflict with each other. A delete whose
// transaction loses a write-write conflict is re-run, and one that keeps losing
// answers a retryable 503; it never answers 500.
func (p *poolLifecycle) concurrentDeletes(ctx context.Context) error {
	if err := p.connect(ctx); err != nil {
		return err
	}
	pools := make([]controller.StoragePool, 0, poolConcurrentDeletes)
	for i := range poolConcurrentDeletes {
		pool, err := p.newPool(ctx, fmt.Sprintf("pool-del-%s-%d", p.a.f.suffix(), i))
		if err != nil {
			return err
		}
		pools = append(pools, pool)
	}
	targets := make([]string, 0, poolConcurrentDeletes+poolDuplicateDeleters-1)
	for _, pool := range pools {
		targets = append(targets, pool.ID)
	}
	for range poolDuplicateDeleters - 1 {
		targets = append(targets, pools[0].ID)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		deleted = map[string]int{}
		bad     []string
	)
	start := make(chan struct{})
	for _, id := range targets {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			err := p.cl.DeleteStoragePool(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			switch status := apiErrorStatus(err); {
			case err == nil:
				deleted[id]++
			case status == http.StatusNotFound || status == http.StatusServiceUnavailable:
				p.a.f.logf("concurrent delete of pool %s: %v", id, err)
			default:
				bad = append(bad, fmt.Sprintf("pool %s: HTTP %d: %v", id, status, err))
			}
		}(id)
	}
	close(start)
	wg.Wait()
	if len(bad) > 0 {
		return fmt.Errorf("%d of %d concurrent pool deletes failed with something other than 204, 404 or a retryable 503 (meko#1849): %s", len(bad), len(targets), strings.Join(bad, "; "))
	}
	for _, pool := range pools {
		if deleted[pool.ID] == 0 {
			if err := p.cl.DeleteStoragePool(ctx, pool.ID); err != nil && apiErrorStatus(err) != http.StatusNotFound {
				return fmt.Errorf("pool %s: no concurrent delete succeeded, and a lone retry failed too: %w", pool.ID, err)
			}
		}
		if _, err := p.cl.GetStoragePool(ctx, pool.ID); apiErrorStatus(err) != http.StatusNotFound {
			return fmt.Errorf("pool %s is still readable after the concurrent deletes (%v), want 404", pool.ID, err)
		}
		if poolListed(ctx, p.cl, pool.ID) {
			return fmt.Errorf("pool %s is still listed after the concurrent deletes", pool.ID)
		}
	}
	return nil
}
