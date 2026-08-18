package launch

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/local/config"
)

// BranchDrop stops+destroys every endpoint on the branch, then tombstones the
// branch (idempotent). The branch's pageserver/safekeeper rows in YB are reclaimed
// asynchronously by the controller's branch-drop reaper, one
// --branch-gc-retention window later; the endpoints are stopped first, so nothing
// is left running over rows that reaper deletes.
func BranchDrop(ctx context.Context, env *config.LocalEnv, branchID config.BranchID) error {
	// Canonicalize before anything compares it: an endpoint's stored branch_id is
	// what the API printed, so a caller's non-canonical spelling would match no
	// endpoint here and still resolve below — dropping the branch with its
	// computes left running.
	id, err := parseBranchID(branchID)
	if err != nil {
		return err
	}
	branchID = config.BranchID(id.String())

	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return err
	}
	// Over `all`: dropping a branch must destroy every endpoint on it, including
	// one whose name is also used on another branch and so is absent from the
	// by-name index.
	for _, ep := range endpoints.all {
		if config.SameBranch(ep.conf.BranchID, branchID) {
			epID := ep.conf.EndpointID
			if err := stopEndpoint(ctx, env, ep, StopModeImmediate, true); err != nil {
				fmt.Fprintf(os.Stderr, "stop+destroy endpoint %s: %v\n", epID, err)
			}
		}
	}
	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}
	branch, err := b.resolveBranch(ctx, branchID)
	if err != nil {
		return err
	}
	// Idempotent: a branch already gone (or never created) is not an error.
	if branch == nil {
		return nil
	}
	// Scoped to the branch's own cluster: a lineage is a cluster, so different
	// branches live in different ones.
	task, err := b.api.DeleteBranch(ctx, b.poolID, branch.ClusterId, id)
	if err != nil {
		return err
	}
	return awaitTask(ctx, b.api, task.Id, branchTaskTimeout)
}

// BranchCreateArgs are the flags of `branch create`.
type BranchCreateArgs struct {
	AncestorBranchID string // required: the branch to fork
	BranchName       string // required: name for the new branch, unique in its cluster
	AncestorStartLSN string // optional; defaults to the ancestor's durable WAL end
}

// BranchCreate forks an existing branch. That is the only thing it does — a
// lineage with no ancestor is a cluster, so `cluster create` makes those.
//
// The fork lands in the ANCESTOR's cluster, since a fork stays in its ancestor's
// lineage. The controller assigns the new branch's id; it is read back and
// printed on the line the harness scrapes.
func BranchCreate(ctx context.Context, env *config.LocalEnv, args BranchCreateArgs) error {
	if args.AncestorBranchID == "" {
		return fmt.Errorf("`branch create` requires --ancestor-branch-id (use `cluster create` for a new lineage)")
	}
	if args.BranchName == "" {
		return fmt.Errorf("`branch create` requires --branch-name")
	}
	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}

	ancestorID := config.BranchID(args.AncestorBranchID)
	ancestor, err := b.resolveBranch(ctx, ancestorID)
	if err != nil {
		return err
	}
	if ancestor == nil {
		return fmt.Errorf("no live branch %s in any of the launcher's clusters", ancestorID)
	}
	// Use the resolved branch's own spelling from here: the lookups below key on
	// the string, and `resolveBranch` accepts any spelling a uuid parses from.
	ancestorID = config.BranchID(ancestor.BranchId.String())

	// The ancestor has to be materialized: the fork point comes from its safekeeper.
	if env.PageserverConfForBranch(ancestorID) == nil {
		return fmt.Errorf("no pageserver serves ancestor branch %s", ancestorID)
	}
	startLSN := args.AncestorStartLSN
	if startLSN == "" {
		startLSN, err = ancestorDurableLSN(ctx, env, ancestorID)
		if err != nil {
			return fmt.Errorf("fetch ancestor durable WAL end from safekeeper: %w", err)
		}
	}

	// The fork point is passed explicitly: the API would otherwise resolve it from
	// the parent's writer endpoint's safekeeper via the deployer, and the launcher
	// has none — but it has just read the durable LSN from that same safekeeper.
	// pg_version is not passed; a fork inherits its ancestor's, resolved server-side.
	ancestorUUID := uuid.UUID(ancestor.BranchId)
	task, err := b.api.CreateBranch(ctx, b.poolID, ancestor.ClusterId, args.BranchName, &ancestorUUID, startLSN)
	if err != nil {
		return err
	}
	if err := awaitTask(ctx, b.api, task.Id, branchTaskTimeout); err != nil {
		return err
	}

	created, err := b.resolveBranchInCluster(ctx, ancestor.ClusterId, args.BranchName)
	if err != nil {
		return err
	}
	if created == nil {
		return fmt.Errorf("branch task succeeded but branch %q is not listed", args.BranchName)
	}
	branchID := created.BranchId.String()

	fmt.Printf(
		"Created branch '%s' (branch %s) at %s from %s; its pageserver + safekeeper come up at `endpoint create`\n",
		args.BranchName, branchID, startLSN, ancestorID)
	// NB: harness invariant — the 2nd whitespace token is the branch id.
	fmt.Println(successfullyCreatedLine(config.BranchID(branchID)))
	return nil
}

// successfullyCreatedLine renders the stdout line the integration harness
// scrapes for a new branch id (cluster.rs: find the line containing
// "successfully created", take the 2nd whitespace token as the branch id).
func successfullyCreatedLine(branchID config.BranchID) string {
	return fmt.Sprintf("branch %s successfully created", branchID)
}
