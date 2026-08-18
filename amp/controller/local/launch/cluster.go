package launch

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/controller/local/config"
)

// ClusterCreateArgs are the flags of `cluster create`.
type ClusterCreateArgs struct {
	Name      string // required; also names the cluster's lineage
	PgVersion int    // Postgres major for the cluster's default branch
}

// ClusterCreate creates a cluster — one independent lineage. Its default branch
// is `main`, is the root of that lineage (its own initdb, its own GC cutoff), and
// is what `branch create` forks from.
//
// This is the only way to make a root branch: /api/v1 has no "branch with no
// parent" — an omitted parent means "fork the cluster's default branch" — because
// in the product model a lineage IS a cluster. The launcher says the same thing
// rather than inventing a second notion of a root.
//
// Prints the `branch <id> successfully created` line the harness scrapes, so a
// caller gets the lineage's branch id without a second lookup.
func ClusterCreate(ctx context.Context, env *config.LocalEnv, args ClusterCreateArgs) error {
	if args.Name == "" {
		return fmt.Errorf("`cluster create` requires a name")
	}
	pgVersion := args.PgVersion
	if pgVersion == 0 {
		pgVersion = config.DefaultPgVersion
	}

	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}
	task, err := b.api.CreateCluster(ctx, b.poolID, args.Name, pgVersion)
	if err != nil {
		return err
	}
	if err := awaitTask(ctx, b.api, task.Id, clusterCreateTimeout); err != nil {
		return err
	}
	cluster, err := b.api.FindCluster(ctx, b.poolID, args.Name)
	if err != nil {
		return err
	}
	if cluster == nil {
		return fmt.Errorf("create-cluster task succeeded but cluster %q is not listed", args.Name)
	}
	branchID := cluster.DefaultBranch.BranchId.String()

	fmt.Printf("Created cluster '%s' (branch %s)\n", args.Name, branchID)
	// NB: harness invariant — the 2nd whitespace token is the branch id.
	fmt.Println(successfullyCreatedLine(config.BranchID(branchID)))
	return nil
}
