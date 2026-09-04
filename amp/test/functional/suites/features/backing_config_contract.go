// The dev foundation ran with auto analyze off, so amp_storage.pages was never
// ANALYZEd and the branch-drop reaper's delete by ybctid (branchgc.go) planned
// as a SeqScan taking 20 minutes per batch (meko#970). AMP depends on these
// YugabyteDB settings and nothing checked them.
package features

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// defaultBackingContract is the settings AMP relies on the foundation for:
// "guc:" entries are read from pg_settings, "gflag:" entries from a tserver's
// /varz (the auto analyze service is a tserver flag with no GUC mirror).
var defaultBackingContract = []string{
	"guc:yb_enable_cbo=on",
	"gflag:ysql_enable_auto_analyze=true",
	// The GC deletes by ybctid carry a TidScan hint; with hints off they plan as
	// SeqScans again. The plan itself is asserted where the statement is built
	// (controller branchgc + pageserver page_store tests), not from a copy here.
	"guc:pg_hint_plan.enable_hint=on",
}

// backingKubeEnv is the standing env's handle on the backing namespace.
type backingKubeEnv interface {
	BackingKube() *k8s.Kubectl
}

var backingTserverSelectors = []string{"app=yb-tserver", "app.kubernetes.io/name=yb-tserver", "app=yugabyted"}

func init() {
	core.Register(core.Test{
		Name:        "features/backing_config_contract",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Assert the backing YugabyteDB carries the settings AMP's storage schema depends on: the cost-based optimizer and pg_hint_plan on (pg_settings on the amp storage database) and the auto analyze service on (a tserver's /varz).",
		Params:      []string{"functional.backing_contract", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runBackingConfigContract,
	})
}

func runBackingConfigContract(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"backing_config_contract", a.backingConfigContract}}
	})
}

func (a *ampFeatureRun) backingConfigContract(ctx context.Context) error {
	db, err := a.f.ampStorageSQL(ctx)
	if err != nil {
		if errors.Is(err, errNoStorageRoute) {
			a.f.t.Skip(err.Error())
			return nil
		}
		return core.AsEnvFailure(fmt.Errorf("connect the backing amp storage database: %w", err))
	}
	defer db.Close()
	want := a.f.t.Options.GetStringSlice("functional.backing_contract.settings", defaultBackingContract)
	var wrong []string
	var flags map[string]string
	for _, entry := range want {
		source, kv, ok := strings.Cut(entry, ":")
		if !ok {
			return fmt.Errorf("functional.backing_contract.settings entry %q is not source:name=value", entry)
		}
		name, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("functional.backing_contract.settings entry %q is not source:name=value", entry)
		}
		var got string
		switch source {
		case "guc":
			known, err := db.QueryInt(ctx, "SELECT count(*) FROM pg_settings WHERE name = $1", name)
			if err != nil {
				return fmt.Errorf("look up %s in pg_settings: %w", name, err)
			}
			if known == 0 {
				wrong = append(wrong, name+" is not a setting this YugabyteDB knows")
				continue
			}
			if got, err = db.QueryString(ctx, "SELECT setting FROM pg_settings WHERE name = $1", name); err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
		case "gflag":
			if flags == nil {
				if flags, err = a.tserverFlags(ctx); err != nil {
					return err
				}
			}
			got, ok = flags[name]
			if !ok {
				wrong = append(wrong, name+" is not a flag the tserver reports")
				continue
			}
		default:
			return fmt.Errorf("functional.backing_contract.settings entry %q: source must be guc or gflag", entry)
		}
		a.f.logf("backing %s %s = %s", source, name, got)
		if got != val {
			wrong = append(wrong, fmt.Sprintf("%s=%s, want %s", name, got, val))
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("the backing YugabyteDB is not configured the way AMP depends on (meko#970: auto analyze off left amp_storage.pages unanalyzed and GC deletes planned as 20-minute SeqScans): %s", strings.Join(wrong, "; "))
	}
	return nil
}

// tserverFlags reads one tserver's effective gflags from its /varz page.
func (a *ampFeatureRun) tserverFlags(ctx context.Context) (map[string]string, error) {
	be, ok := a.f.ampEnv.(backingKubeEnv)
	if !ok || be.BackingKube() == nil {
		a.f.t.Skip("no kube handle for the backing namespace, so tserver gflags cannot be read")
		return nil, nil
	}
	kube := be.BackingKube()
	var pod string
	for _, sel := range backingTserverSelectors {
		names, err := kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.name}")
		if err != nil {
			return nil, fmt.Errorf("list backing pods %s: %w", sel, err)
		}
		if f := strings.Fields(names); len(f) > 0 {
			pod = f[0]
			break
		}
	}
	if pod == "" {
		return nil, core.EnvFail("no tserver pod found in namespace %q", kube.Namespace())
	}
	// curl through kubectl exec fails transiently and the tserver image has no
	// wget to fall back on, so retry the same call.
	deadline := time.Now().Add(varzReadBudget)
	for {
		res, err := kube.ForPod(pod, "").Run(ctx, `curl -sf 'http://127.0.0.1:9000/varz?raw'`)
		if err == nil && res.ExitCode == 0 {
			return parseVarz(res.Stdout), nil
		}
		if err == nil {
			err = fmt.Errorf("curl exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("read /varz from %s: %w", pod, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("read /varz from %s: %w", pod, err)
		case <-time.After(varzReadInterval):
		}
	}
}

const (
	varzReadBudget   = 30 * time.Second
	varzReadInterval = 2 * time.Second
)

// parseVarz turns "--flag=value" lines into a map.
func parseVarz(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "--") {
			continue
		}
		if k, v, ok := strings.Cut(strings.TrimPrefix(line, "--"), "="); ok {
			out[k] = v
		}
	}
	return out
}
