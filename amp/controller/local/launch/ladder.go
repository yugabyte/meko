package launch

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
)

// The launcher's own resource ladder.
//
// There is one branch table and one endpoint table, shared by the product API and
// the storage-layer control surface, and they are project-scoped — so the
// launcher needs a real project before it can declare a branch. It provisions one
// through /api/v1 the first time the controller comes up and remembers its id in
// the config, which is what makes a `yb_amp_local` branch an ordinary branch the
// product API can list rather than a row in a parallel registry.
//
// The user and pool ids are fixed rather than generated. Both create endpoints
// are idempotent on an explicit id, so a fixed one converges even against a
// database that outlives the config dir — and there is exactly one launcher
// identity per machine, so there is nothing to disambiguate. The project cannot
// do the same (its create is async and mints its own id), so it is resolved by
// name instead.
const (
	// LauncherUserID owns everything the launcher creates.
	LauncherUserID = "a11c0000-0000-0000-0000-0000000000d1"
	// LauncherPoolID is the launcher's storage pool.
	LauncherPoolID = "a11c0000-0000-0000-0000-0000000000d2"
	// LauncherPoolName / LauncherProjectName name them for a human reading the
	// admin console.
	LauncherPoolName    = "yb-amp-local"
	LauncherProjectName = "local"
)

// projectCreateTimeout bounds how long EnsureLadder waits for the create-project
// task. It is a couple of task-worker poll intervals plus the two inserts the
// task actually does; nothing in it waits on a pod.
const projectCreateTimeout = 60 * time.Second

// EnsureLadder makes sure the launcher's user, storage pool, and project exist,
// and records the project id (and its default branch) in env. Idempotent: a
// second call against an already-provisioned repo re-resolves the same rows and
// only persists if something changed.
//
// It must run after the controller answers /ready and before anything drives
// /control/v1. Returns whether env was mutated, so the caller can persist.
func EnsureLadder(ctx context.Context, env *config.LocalEnv) (bool, error) {
	base, err := controllerBase(env)
	if err != nil {
		return false, err
	}
	apiClient := client.NewAPI(base, httpClient())

	userID, err := uuid.Parse(LauncherUserID)
	if err != nil {
		return false, fmt.Errorf("parse launcher user id: %w", err)
	}
	poolID, err := uuid.Parse(LauncherPoolID)
	if err != nil {
		return false, fmt.Errorf("parse launcher pool id: %w", err)
	}

	if err := apiClient.EnsureUser(ctx, userID, true); err != nil {
		return false, fmt.Errorf("ensure launcher user: %w", err)
	}
	if err := apiClient.EnsureStoragePool(ctx, poolID, userID, LauncherPoolName); err != nil {
		return false, fmt.Errorf("ensure launcher storage pool: %w", err)
	}

	project, err := apiClient.FindProject(ctx, poolID, LauncherProjectName)
	if err != nil {
		return false, fmt.Errorf("look up launcher project: %w", err)
	}
	if project == nil {
		fmt.Printf("Creating the launcher's control-plane project %q\n", LauncherProjectName)
		task, err := apiClient.CreateProject(ctx, poolID, LauncherProjectName, config.DefaultPgVersion)
		if err != nil {
			return false, fmt.Errorf("create launcher project: %w", err)
		}
		if err := awaitTask(ctx, apiClient, task.Id, projectCreateTimeout); err != nil {
			return false, fmt.Errorf("create launcher project: %w", err)
		}
		if project, err = apiClient.FindProject(ctx, poolID, LauncherProjectName); err != nil {
			return false, fmt.Errorf("re-resolve launcher project: %w", err)
		}
		if project == nil {
			return false, fmt.Errorf("create-project task succeeded but project %q is not listed", LauncherProjectName)
		}
	}

	// The project always comes with a default branch named "main". Adopt it as the
	// launcher's default rather than creating a second root: `branch create` with
	// no name is exactly a request for this branch, and a second one would collide
	// on branches_name_per_project anyway.
	mutated := false
	if env.ProjectID != project.Id.String() {
		env.ProjectID = project.Id.String()
		mutated = true
	}
	defaultBranch := config.BranchID(hex32UUID(project.DefaultBranch.BranchId))
	if env.DefaultBranchID == nil || *env.DefaultBranchID != defaultBranch {
		env.DefaultBranchID = &defaultBranch
		mutated = true
	}
	return mutated, nil
}

// awaitTask polls a task to a terminal status, failing on `failed` and on
// timeout. The controller's mutations are async (202 + task), so this is how a
// synchronous CLI waits for one.
func awaitTask(ctx context.Context, api *client.API, taskID uuid.UUID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		task, err := api.GetTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("poll task %s: %w", taskID, err)
		}
		switch task.Status {
		case "succeeded":
			return nil
		case "failed":
			msg := "no error recorded"
			if task.Error != nil {
				msg = *task.Error
			}
			return fmt.Errorf("task %s failed: %s", taskID, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s did not finish within %s (status %s)", taskID, timeout, task.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// projectID returns the launcher's provisioned project id, or a diagnostic
// pointing at `yb_amp_local start` when the repo has not been bootstrapped.
func projectID(env *config.LocalEnv) (uuid.UUID, error) {
	if env.ProjectID == "" {
		return uuid.Nil, fmt.Errorf(
			"this repo has no control-plane project yet; run `yb_amp_local start` (it provisions one) before creating branches or endpoints")
	}
	id, err := uuid.Parse(env.ProjectID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("config has a malformed project_id %q: %w", env.ProjectID, err)
	}
	return id, nil
}

// controllerClient builds the project-scoped storage-layer client every branch
// and endpoint command drives.
func controllerClient(env *config.LocalEnv) (*client.Controller, error) {
	base, err := controllerBase(env)
	if err != nil {
		return nil, err
	}
	pid, err := projectID(env)
	if err != nil {
		return nil, err
	}
	return client.NewController(base, pid, httpClient()), nil
}

// hex32UUID renders a uuid as the storage layer's flat 32-lowercase-hex BranchId,
// matching the controller's own rendering at the storage seam.
func hex32UUID(id uuid.UUID) string {
	return fmt.Sprintf("%x", [16]byte(id))
}

// persistLadder writes env back to disk after EnsureLadder mutated it, reporting
// a failure to stderr rather than aborting startup: the ids are re-resolvable on
// the next run, so a config write failure is not fatal to a cluster that is
// already up.
func persistLadder(env *config.LocalEnv) {
	if err := env.PersistConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist the launcher project id: %v\n", err)
	}
}
