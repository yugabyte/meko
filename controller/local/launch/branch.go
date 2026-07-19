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
	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	return client.NewController(base, httpClient()).BranchDrop(ctx, branchID)
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

// branchCreateRoot records a pending Bootstrap init for a new root branch.
func branchCreateRoot(ctx context.Context, env *config.LocalEnv, args BranchCreateArgs) error {
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

	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	ctrl := client.NewController(base, httpClient())
	if err := ctrl.RecordPendingInit(ctx, branchID, registry.PendingBranchInitRequest{
		BranchName: DefaultBranchName,
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

	base, err := controllerBase(env)
	if err != nil {
		return err
	}
	ctrl := client.NewController(base, httpClient())

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
	if err := ctrl.RecordPendingInit(ctx, branchID, registry.PendingBranchInitRequest{
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
