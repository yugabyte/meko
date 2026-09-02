// amp-dev ran a cassert, -O0 Postgres for weeks (meko#1019) and the first AMP vs
// Aurora numbers were taken on it. Nothing asserted the image under test is a
// release build; this does.
package features

import (
	"context"
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// releaseBuildProbe finds the running postmaster's install and prints how it was
// configured and compiled.
const releaseBuildProbe = `d=$(dirname "$(readlink -f /proc/$(pgrep -xo postgres)/exe)"); echo "configure: $("$d/pg_config" --configure)"; echo "cflags: $("$d/pg_config" --cflags)"`

func init() {
	core.Register(core.Test{
		Name:        "features/amp_release_build",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Assert the endpoint image ships a release Postgres: debug_assertions off, no --enable-cassert in pg_config --configure, no -O0 in its cflags.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpReleaseBuild,
	})
}

func runAmpReleaseBuild(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"release_build", a.releaseBuild}}
	})
}

func (a *ampFeatureRun) releaseBuild(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return err
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer c.Close()
	asserts, err := c.QueryString(ctx, "SHOW debug_assertions")
	if err != nil {
		return fmt.Errorf("SHOW debug_assertions: %w", err)
	}
	if asserts != "off" {
		return fmt.Errorf("debug_assertions is %q on the endpoint: the image ships a cassert Postgres, which runs about 3x slower than a release build (meko#1019)", asserts)
	}
	kube := a.f.ampKube()
	if kube == nil {
		a.f.logf("no kube handle for the endpoint pod, so pg_config is not inspected; debug_assertions=off is the only release-build evidence")
		return nil
	}
	roller := &kubeComputeRoller{kube: kube, timeout: restartReadyTimeout}
	pod, err := roller.podName(ctx, ep.ID)
	if err != nil {
		return err
	}
	res, err := kube.ForPod(pod, "").Run(ctx, releaseBuildProbe)
	if err != nil {
		return fmt.Errorf("probe pg_config in pod %s: %w", pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pg_config probe in pod %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return assertReleaseConfigure(res.Stdout)
}

// assertReleaseConfigure judges the two pg_config lines the probe prints.
func assertReleaseConfigure(out string) error {
	if strings.Contains(out, "--enable-cassert") {
		return fmt.Errorf("the endpoint's Postgres was configured with --enable-cassert; release images must not carry assertions (meko#1019): %s", strings.TrimSpace(out))
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "cflags:") {
			for _, f := range strings.Fields(line) {
				if f == "-O0" {
					return fmt.Errorf("the endpoint's Postgres was compiled at -O0: %s", strings.TrimSpace(line))
				}
			}
		}
	}
	if !strings.Contains(out, "configure:") {
		return fmt.Errorf("pg_config probe printed no configure line: %q", out)
	}
	return nil
}
