package launch

import (
	"context"
	"fmt"
	"os"

	"github.com/yugabyte/amp/controller/internal/registry"
	"github.com/yugabyte/amp/controller/local/client"
	"github.com/yugabyte/amp/controller/local/config"
)

// BranchDrop stops+destroys every endpoint on the branch, then marks the branch
// dropped in the controller (idempotent; the pageserver/safekeeper YB data is
// left for later GC). Mirrors Rust timeline_drop.
func BranchDrop(ctx context.Context, env *config.LocalEnv, branchID config.BranchID) error {
	endpoints, err := loadEndpoints(ctx, env)
	if err != nil {
		return err
	}
	for id, ep := range endpoints {
		if ep.BranchID == branchID {
			if err := EndpointStop(ctx, env, id, StopModeImmediate, true); err != nil {
				fmt.Fprintf(os.Stderr, "stop+destroy endpoint %s: %v\n", id, err)
			}
		}
	}
	ctrl, err := controllerClient(env)
	if err != nil {
		return err
	}
	return ctrl.BranchDrop(ctx, branchID)
}

// BranchCreateArgs are the flags of `branch create`.
type BranchCreateArgs struct {
	BranchID           string // optional; generated if empty
	BranchName         string // optional; defaults to "main" for a root branch
	AncestorBranchName string // set => fork; unset => root
	AncestorStartLSN   string // optional; fork only
	PgVersion          int    // root branch only (a fork inherits its ancestor's)
	SetDefault         bool   // root branch only
}

// BranchCreate creates a root branch (no ancestor) or a fork. It records a
// pending init in the controller and prints the `branch <id> successfully
// created` line the integration harness scrapes (the branch id is the 2nd
// whitespace token). Mirrors Rust `handle_branch` + the root/fork bodies of
// `handle_tenant`/`handle_timeline`. Returns whether env was mutated
// (--set-default on a root branch).
func BranchCreate(ctx context.Context, env *config.LocalEnv, args BranchCreateArgs) error {
	if args.AncestorBranchName != "" {
		return branchCreateFork(ctx, env, args)
	}
	return branchCreateRoot(ctx, env, args)
}

// branchCreateRoot declares a new root branch — one with no ancestor, which the
// first endpoint materializes by running initdb.
//
// The unqualified form (`branch create` with no name and no id) is a request for
// the repo's `main`, and that branch already exists: the launcher's project comes
// with a default branch of that name, provisioned by EnsureLadder. So this adopts
// it rather than declaring a second root, which would collide on
// branches_name_per_project anyway. Naming a root branch explicitly, or pinning its
// id, still declares a new one alongside `main`.
func branchCreateRoot(ctx context.Context, env *config.LocalEnv, args BranchCreateArgs) error {
	name := args.BranchName
	if name == "" {
		name = DefaultBranchName
	}

	if args.BranchID == "" && name == DefaultBranchName && env.DefaultBranchID != nil {
		adopted := *env.DefaultBranchID
		// NB: harness invariant — the 2nd whitespace token is the branch id.
		fmt.Println(successfullyCreatedLine(adopted))
		fmt.Printf("Adopted the project's default branch '%s' as '%s'\n", adopted, name)
		return nil
	}

	branchID := args.BranchID
	if branchID == "" {
		id, err := generateBranchID()
		if err != nil {
			return err
		}
		branchID = id
	}
	pgVersion := args.PgVersion
	if pgVersion == 0 {
		pgVersion = config.DefaultPgVersion
	}

	ctrl, err := controllerClient(env)
	if err != nil {
		return err
	}
	if err := ctrl.RecordBranch(ctx, branchID, registry.BranchCreateRecord{
		BranchName: name,
		PgVersion:  pgVersion,
		StartLSN:   "0/0",
	}); err != nil {
		return err
	}

	// NB: harness invariant — the 2nd whitespace token is the branch id.
	fmt.Println(successfullyCreatedLine(branchID))
	fmt.Printf("Created an initial branch '%s'\n", branchID)

	if args.SetDefault {
		fmt.Printf("Setting branch %s as a default one\n", branchID)
		id := branchID
		env.DefaultBranchID = &id
	}
	return nil
}

// branchCreateFork records a pending Branch init off the named ancestor,
// resolving the ancestor's branch point from its safekeeper's durable LSN.
func branchCreateFork(ctx context.Context, env *config.LocalEnv, args BranchCreateArgs) error {
	if args.BranchName == "" {
		return fmt.Errorf("`branch create` with --ancestor-branch-name requires --branch-name")
	}
	branchID := args.BranchID
	if branchID == "" {
		id, err := generateBranchID()
		if err != nil {
			return err
		}
		branchID = id
	}

	ctrl, err := controllerClient(env)
	if err != nil {
		return err
	}

	ancestorBranchID, err := getBranchTimelineID(ctx, env, ctrl, args.AncestorBranchName)
	if err != nil {
		return err
	}
	if ancestorBranchID == "" {
		return fmt.Errorf("Found no branch id for branch name '%s'", args.AncestorBranchName)
	}

	// The ancestor is running: read its pg_version from its pageserver and
	// its durable branch point from its safekeeper.
	ancestorConf := env.PageserverConfForBranch(ancestorBranchID)
	if ancestorConf == nil {
		return fmt.Errorf("no pageserver serves ancestor branch %s", ancestorBranchID)
	}
	info, err := client.NewPageserver(ancestorConf.ListenHTTPAddr, httpClient()).TimelineInfo(ctx, ancestorBranchID)
	if err != nil {
		return fmt.Errorf("fetch ancestor branch info: %w", err)
	}
	pgVersion := info.PgVersion

	startLSN := args.AncestorStartLSN
	if startLSN == "" {
		startLSN, err = ancestorDurableLSN(ctx, env, ancestorBranchID)
		if err != nil {
			return fmt.Errorf("fetch ancestor durable WAL end from safekeeper: %w", err)
		}
	}

	anc := ancestorBranchID
	if err := ctrl.RecordBranch(ctx, branchID, registry.BranchCreateRecord{
		BranchName:       args.BranchName,
		PgVersion:        pgVersion,
		AncestorBranchID: &anc,
		StartLSN:         startLSN,
	}); err != nil {
		return err
	}

	fmt.Printf(
		"Created branch '%s' (branch %s) at %s from '%s'; its pageserver + safekeeper come up at `endpoint create`\n",
		args.BranchName, branchID, startLSN, args.AncestorBranchName)
	// Keep the harness invariant consistent across root and fork.
	fmt.Println(successfullyCreatedLine(branchID))
	return nil
}

// successfullyCreatedLine renders the stdout line the integration harness
// scrapes for a new branch id (cluster.rs: find the line containing
// "successfully created", take the 2nd whitespace token as the branch id).
func successfullyCreatedLine(branchID config.BranchID) string {
	return fmt.Sprintf("branch %s successfully created", branchID)
}

// getBranchTimelineID resolves a branch name to its id via the controller's
// branch-name registry, preferring a locally-served branch when several
// share the name. Mirrors Rust `LocalEnv::get_branch_timeline_id`.
func getBranchTimelineID(ctx context.Context, env *config.LocalEnv, ctrl *client.Controller, branchName string) (config.BranchID, error) {
	entries, err := ctrl.ListBranchNames(ctx)
	if err != nil {
		return "", err
	}
	var matches []config.BranchID
	for _, e := range entries {
		if e.BranchName == branchName {
			matches = append(matches, e.BranchID)
		}
	}
	// Prefer the branch this launcher actually serves (scan pageservers in
	// provisioning order), else the first controller match.
	for _, ps := range env.Pageservers {
		if ps.BranchID == nil {
			continue
		}
		for _, m := range matches {
			if m == *ps.BranchID {
				return m, nil
			}
		}
	}
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", nil
}
