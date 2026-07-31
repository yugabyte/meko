// Package tasks holds the built-in ops routines (architecture §4.2 Task,
// implementation §3 M3): reap, collect-logs, deploy-artifact. Each registers
// itself in task's default registry via init() — blank-import this package to
// pull them all in (cmd/ampqa does).
package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/cloud"
	"github.com/yugabyte/amp/test/functional/framework/apps/dockerhost"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	sshx "github.com/yugabyte/amp/test/functional/framework/exec/ssh"
	"github.com/yugabyte/amp/test/functional/framework/task"
)

func init() {
	task.Register(reapTask)
	task.Register(collectLogsTask)
	task.Register(deployArtifactTask)
}

// sshTransport is what reap/collect-logs need from a connection:
// exec.RemoteExecutor plus Close. *ssh.Client satisfies it; tests inject a
// scripted fake.
type sshTransport interface {
	exec.RemoteExecutor
	Close() error
}

// dialFunc dials an ssh host; overridden in tests to avoid real networking.
type dialFunc func(ctx context.Context, host, user, keyPath string) (sshTransport, error)

func defaultDial(ctx context.Context, host, user, keyPath string) (sshTransport, error) {
	return sshx.Dial(ctx, sshx.Config{Host: host, User: user, KeyPath: keyPath})
}

// ReapOptions parameterizes one reap pass; dependency-injected so unit tests
// run with no real network or wall-clock dependence.
type ReapOptions struct {
	Now    func() time.Time
	Dial   dialFunc
	DryRun bool
	Logf   func(format string, args ...any)
	// Execute runs a recorded reap.Plan (k8s namespaces + cloud tag-reap) — the
	// SAME code cmd/ampqa's env rm uses, so there is one teardown implementation
	// per shape. The reapTask wires it from the task's config + secret resolver;
	// tests inject a fake. nil → an entry carrying a plan cannot be reaped and its
	// record is KEPT (never silently dropped).
	Execute func(ctx context.Context, plan *reap.Plan) error
	// CloudReap terminates a run tag's instances for a bare-tag cloud entry that
	// carries no plan. Default builds the cloud client; tests inject a fake. nil →
	// such an entry cannot be reaped and its record is KEPT.
	CloudReap func(ctx context.Context, cloudName, runTag string) error
}

// ReapResult reports what one pass did.
type ReapResult struct {
	Expired      []string // entry ids past TTL
	Removed      []string // entry ids actually removed from the registry
	TeardownErrs map[string]error
}

// Reap scans reg for entries past their TTL and reclaims their real
// infrastructure, then removes the record ONLY when the reclaim succeeded. For
// every expired entry it runs the strongest teardown the record supports —
// a recorded reap.Plan (k8s namespaces + cloud tag-reap) via opts.Execute, else
// a `docker`-shape ssh StopRm, else a bare cloud tag-reap via opts.CloudReap —
// and KEEPS the record with a loud error on any failure, so a leak is never
// dropped from the registry. An entry that records no infrastructure handle at
// all (no plan, no docker host, no run tag) has nothing to reclaim and its stale
// record is removed.
func Reap(ctx context.Context, reg *registry.Registry, opts ReapOptions) (ReapResult, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	dial := dialFunc(defaultDial)
	if opts.Dial != nil {
		dial = opts.Dial
	}
	logf := func(string, ...any) {}
	if opts.Logf != nil {
		logf = opts.Logf
	}

	entries, err := reg.List()
	if err != nil {
		return ReapResult{}, fmt.Errorf("list registry: %w", err)
	}
	res := ReapResult{TeardownErrs: map[string]error{}}
	t := now()
	for _, e := range entries {
		if !e.Expired(t) {
			continue
		}
		res.Expired = append(res.Expired, e.ID)
		if opts.DryRun {
			continue
		}
		if terr := reclaim(ctx, e, dial, opts); terr != nil {
			// A failed reclaim means the infrastructure may still be alive: KEEP
			// the record so the leak stays discoverable and a later pass retries.
			res.TeardownErrs[e.ID] = terr
			logf("reap: teardown of %s FAILED — record kept, infrastructure may survive: %v", e.ID, terr)
			continue
		}
		removed, err := reg.Remove(e.ID)
		if err != nil {
			return res, fmt.Errorf("remove %s: %w", e.ID, err)
		}
		if removed {
			res.Removed = append(res.Removed, e.ID)
		}
	}
	return res, nil
}

// reclaim tears down one expired entry's real infrastructure using the strongest
// mechanism its record supports, returning an error (record must be kept) or nil
// (record may be removed). An entry that carries a plan/tag but no executor is a
// failure — the reaper cannot honor its own no-leak guarantee, so the record is
// kept rather than silently dropped.
func reclaim(ctx context.Context, e registry.Entry, dial dialFunc, opts ReapOptions) error {
	plan, err := reap.FromJSON(e.Data["reap_plan"])
	if err != nil {
		return fmt.Errorf("decode reap plan: %w", err)
	}
	switch {
	case !plan.Empty():
		if opts.Execute == nil {
			return fmt.Errorf("entry carries a reap plan but no plan executor is configured")
		}
		return opts.Execute(ctx, plan)
	case e.Provider == "docker" && e.Data["host"] != "":
		return teardownDocker(ctx, e, dial)
	case e.Data["run_tag"] != "" && (e.Data["cloud"] == "aws" || e.Data["cloud"] == "gcp"):
		if opts.CloudReap == nil {
			return fmt.Errorf("entry carries a cloud run tag but no cloud reaper is configured")
		}
		return opts.CloudReap(ctx, e.Data["cloud"], e.Data["run_tag"])
	default:
		// No recorded infrastructure handle: nothing to reclaim, safe to drop.
		return nil
	}
}

func teardownDocker(ctx context.Context, e registry.Entry, dial dialFunc) error {
	host := e.Data["host"]
	if host == "" {
		return fmt.Errorf("docker entry %s: no host in data", e.ID)
	}
	tr, err := dial(ctx, host, e.Data["ssh_user"], e.Data["ssh_key"])
	if err != nil {
		return fmt.Errorf("dial %s: %w", host, err)
	}
	defer tr.Close()
	dh := dockerhost.New(tr)

	var firstErr error
	for _, raw := range strings.Split(e.Data["containers"], ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if err := dh.StopRm(ctx, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// reapTask is the registry-TTL enforcement entrypoint, reachable as
// `ampqa task reap`.
//
// OPERATIONAL CONTRACT — TTL enforcement is NOT automatic. The framework is a
// short-lived CLI: it never runs a background daemon on the operator's machine
// (AGENTS.md §4), so nothing sweeps expired entries on its own. A `run` or
// `setup` records a crash-recovery entry with a TTL, but a killed/abandoned env
// past its TTL is only reclaimed when this task runs. Deployments MUST schedule
// it externally — a cron entry, systemd timer, or CI cleanup step, e.g.:
//
//	*/15 * * * *  ampqa task reap
//
// A dry run (`-o task.dry_run=bool:true`) lists what would be reaped without
// touching anything. (Cloud infrastructure whose registry record was lost
// entirely is a separate net: `ampqa env reap --tag/--stale`.)
var reapTask = task.Task{
	Name: "reap",
	Doc:  "Reclaim standing-registry entries past their TTL (docker/k8s/cloud teardown), then drop the record. NOT automatic — schedule it (cron/systemd/CI); nothing sweeps expired envs on its own.",
	Params: map[string]string{
		"task.registry": "registry file path (default ~/.ampqa/envs.json)",
		"task.dry_run":  "bool: list what would be reaped without removing (default false)",
	},
	Run: func(ctx context.Context, deps task.Deps) error {
		path := deps.Config.GetString("task.registry", "")
		if path == "" {
			p, err := registry.DefaultPath()
			if err != nil {
				return err
			}
			path = p
		}
		reg := registry.New(path)
		secrets, err := core.NewSecrets(deps.Config)
		if err != nil {
			return fmt.Errorf("reap: resolve secrets: %w", err)
		}
		res, err := Reap(ctx, reg, ReapOptions{
			DryRun: deps.Config.GetBool("task.dry_run", false),
			Logf:   deps.Logf,
			// Reuse the apps' own teardown code (reap.Execute) and the cloud tag-reap,
			// resolving region/credentials from the task's config + secret resolver.
			Execute: func(ctx context.Context, plan *reap.Plan) error {
				// force=false: a cloud reap that describes zero instances is treated
				// as unconfirmed and the record is kept, not silently dropped.
				return reap.Execute(ctx, plan, deps.Config, secrets.Value, false)
			},
			CloudReap: func(ctx context.Context, cloudName, runTag string) error {
				vm, err := cloud.New(cloudName, deps.Config, secrets.Value)
				if err != nil {
					return fmt.Errorf("build %s client for tag reap: %w", cloudName, err)
				}
				return vm.ReapByTag(ctx, runTag)
			},
		})
		if err != nil {
			return err
		}
		logf := deps.Logf
		if logf == nil {
			logf = func(string, ...any) {}
		}
		logf("reap: %d expired, %d removed, %d teardown error(s)", len(res.Expired), len(res.Removed), len(res.TeardownErrs))
		if len(res.TeardownErrs) > 0 {
			return fmt.Errorf("reap: %d entr(y/ies) could not be reaped and were kept (infrastructure may survive)", len(res.TeardownErrs))
		}
		return nil
	},
}
