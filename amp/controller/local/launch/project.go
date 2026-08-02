package launch

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/controller/local/config"
)

// ProjectCreateArgs are the flags of `project create`.
type ProjectCreateArgs struct {
	Name      string // required; also names the project's lineage
	PgVersion int    // Postgres major for the project's default branch
}

// ProjectCreate creates a project — one independent lineage. Its default branch
// is `main`, is the root of that lineage (its own initdb, its own GC cutoff), and
// is what `branch create` forks from.
//
// This is the only way to make a root branch: /api/v1 has no "branch with no
// parent" — an omitted parent means "fork the project's default branch" — because
// in the product model a lineage IS a project. The launcher says the same thing
// rather than inventing a second notion of a root.
//
// Prints the `branch <id> successfully created` line the harness scrapes, so a
// caller gets the lineage's branch id without a second lookup.
func ProjectCreate(ctx context.Context, env *config.LocalEnv, args ProjectCreateArgs) error {
	if args.Name == "" {
		return fmt.Errorf("`project create` requires a name")
	}
	pgVersion := args.PgVersion
	if pgVersion == 0 {
		pgVersion = config.DefaultPgVersion
	}

	b, err := newBranchAPI(env)
	if err != nil {
		return err
	}
	task, err := b.api.CreateProject(ctx, b.poolID, args.Name, pgVersion)
	if err != nil {
		return err
	}
	if err := awaitTask(ctx, b.api, task.Id, projectCreateTimeout); err != nil {
		return err
	}
	project, err := b.api.FindProject(ctx, b.poolID, args.Name)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("create-project task succeeded but project %q is not listed", args.Name)
	}
	branchID := hex32UUID(project.DefaultBranch.BranchId)

	fmt.Printf("Created project '%s' (branch %s)\n", args.Name, branchID)
	// NB: harness invariant — the 2nd whitespace token is the branch id.
	fmt.Println(successfullyCreatedLine(config.BranchID(branchID)))
	return nil
}
