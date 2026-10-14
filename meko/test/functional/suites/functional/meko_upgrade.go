package functional

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/diag"
	k8senv "github.com/yugabyte/meko/meko/test/functional/framework/env/k8s"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

const (
	seedDatapacks = 3
	pollEvery     = time.Second
	pollTimeout   = 5 * time.Second
)

func init() {
	core.Register(core.Test{
		Name:       "functional/upgrade_previous_release",
		Unguarded:  true,
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags("upgrade"),
		OwningTeam: core.TeamPlatform,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.ClusterOnly, ClusterSlots: 1},
		Timeout:    90 * time.Minute,
		Description: "Install the release before the run's (upgrade.from overrides it) from its own chart and images, seed datapacks, an agent, " +
			"a PAT and a conversation, then helm upgrade in place to the run's chart and images while polling the API. The data, the PAT and " +
			"the Langfuse projects survive, new datapacks work, and the API never stays down longer than upgrade.max_outage.",
		Run: previousRelease,
	})
}

type seeded struct {
	datapacks map[string]string
	agent     string
	pat       mekoapi.Token
	convDP    string
}

func previousRelease(t *core.T) (rerr error) {
	cluster := stacktest.Env(t)
	cfg := t.Options
	target := cfg.GetString("env.mekostack.image_version", "")
	chart, err := mekostack.ResolveChart(cfg.GetString("env.mekostack.chart", ""))
	if err != nil {
		return err
	}
	maxOutage := cfg.GetDuration("upgrade.max_outage", 30*time.Second)

	var from, fromChart, fromImage string
	if err := t.Step("resolve_versions", func(h *core.StepHandle) error {
		root, err := mekostack.RepoRoot(t, chart)
		if err != nil {
			return err
		}
		rel := mekostack.NewReleases(root)
		from = cfg.GetString("upgrade.from", "")
		if from == "" {
			if from, err = rel.Previous(t, mekostack.ReleaseVersion(target)); err != nil {
				return err
			}
		}
		dir, err := os.MkdirTemp("", "mekoqa-upgrade-")
		if err != nil {
			return err
		}
		if fromChart, fromImage, err = rel.Chart(t, from, dir); err != nil {
			return err
		}
		h.Set("from", fromImage)
		h.Set("to", target)
		return nil
	}); err != nil {
		return err
	}

	var env *k8senv.Env
	var teardown func() error
	if err := t.Step("install_from", func(*core.StepHandle) error {
		var err error
		env, teardown, err = cluster.BuildStack(t, map[string]string{
			"env.mekostack.chart":         fromChart,
			"env.mekostack.image_version": fromImage,
			// Released charts run langfuse's migrations only against their own database.
			"db.provider": "chart",
		})
		return err
	}); err != nil {
		return err
	}
	defer func() {
		// The stack is the test's own, gone before the run collects diagnostics.
		mode := cfg.GetString("report.collect_diagnostics", core.DefaultCollectDiagnostics)
		if diag.ShouldCollect(mode, rerr != nil || t.Failed()) {
			ctx, cancel := context.WithTimeout(context.Background(), core.ResolveCollectTimeout(cfg))
			_ = env.Collect(ctx, resultSink{t})
			cancel()
		}
		_ = teardown()
	}()

	apiFor := func() *mekoapi.Client {
		return env.API().WithErrorHook(func(e *mekoapi.Error) {
			stacktest.FailOnLeak(t, e.Method+" "+e.Path, e.Message+" "+e.Body)
		})
	}
	api := apiFor()
	mcp := func(pat string) *mcpclient.Client {
		return env.MCP(pat, mcpclient.WithResultHook(func(tool, text string) { stacktest.FailOnLeak(t, "mcp "+tool, text) }))
	}

	var s seeded
	if err := t.Step("seed", func(h *core.StepHandle) error {
		var err error
		s, err = seed(t, api, mcp)
		h.Set("datapacks", len(s.datapacks))
		return err
	}); err != nil {
		return err
	}

	var poll *poller
	if err := t.Step("upgrade", func(h *core.StepHandle) error {
		// A probe that cannot pass on a serving stack would read as an outage.
		if err := env.Stack().ProbeAPI(t); err != nil {
			return fmt.Errorf("the API probe fails before the upgrade: %w", err)
		}
		poll = startPoller(env.Stack())
		start := time.Now()
		err := env.Stack().Upgrade(t, chart, target)
		poll.stop()
		h.Set("took", time.Since(start).Round(time.Second).String())
		return err
	}); err != nil {
		return err
	}

	api = apiFor()
	var errs []error
	step := func(name string, fn func(*core.StepHandle) error) {
		if err := t.Step(name, fn); err != nil {
			errs = append(errs, err)
		}
	}
	step("availability", func(h *core.StepHandle) error {
		longest, failed, total := poll.result()
		h.Set("failed_polls", failed)
		h.Set("polls", total)
		h.Set("longest_outage", longest.String())
		t.Metric("upgrade.longest_outage_seconds", longest.Seconds(), "s")
		t.Metric("upgrade.failed_polls", float64(failed), "count")
		if longest > maxOutage {
			return fmt.Errorf("the API failed every poll for %s during the upgrade, longer than %s (%d of %d polls failed)", longest, maxOutage, failed, total)
		}
		return nil
	})
	step("running_target", func(h *core.StepHandle) error {
		product, _ := env.Stack().Versions(t)
		h.Set("api_server", product)
		if product != target {
			return fmt.Errorf("the api-server runs %q after the upgrade, want %q", product, target)
		}
		return nil
	})
	step("data_survived", func(*core.StepHandle) error { return checkData(t, api, env.Stack(), s) })
	step("pat_survived", func(*core.StepHandle) error {
		c := mcp(s.pat.Value)
		defer c.Close(context.Background())
		if _, err := c.Initialize(t); err != nil {
			return fmt.Errorf("MCP with the PAT minted before the upgrade: %w", err)
		}
		res, err := c.CallTool(t, "conversation_create", map[string]any{"datapack_id": s.convDP, "title": "after upgrade"})
		if err != nil {
			return err
		}
		if res.IsError {
			return fmt.Errorf("conversation_create on a datapack from before the upgrade: %s", res.Text())
		}
		return nil
	})
	step("new_datapack_works", func(*core.StepHandle) error {
		dp, err := api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: "up-new-" + t.ResourceSuffix()})
		if err != nil {
			return err
		}
		ok, err := stacktest.HasLangfuseProject(t, env.Stack(), dp.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("datapack %s created after the upgrade has no Langfuse project", dp.ID)
		}
		return api.DeleteDatapack(t, dp.ID)
	})
	return errors.Join(errs...)
}

func seed(t *core.T, api *mekoapi.Client, mcp func(string) *mcpclient.Client) (seeded, error) {
	s := seeded{datapacks: map[string]string{}, agent: "up-agent-" + t.ResourceSuffix()}
	if _, err := api.CreateAgent(t, s.agent, nil); err != nil {
		return s, err
	}
	for i := range seedDatapacks {
		name := fmt.Sprintf("up-dp-%d-%s", i, t.ResourceSuffix())
		dp, err := api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: name})
		if err != nil {
			return s, err
		}
		s.datapacks[dp.ID] = name
		if s.convDP == "" {
			s.convDP = dp.ID
		}
	}
	var err error
	if s.pat, err = api.CreateToken(t, mekoapi.CreateTokenRequest{Name: "up-pat-" + t.ResourceSuffix(), TTLDays: 1}); err != nil {
		return s, err
	}
	c := mcp(s.pat.Value)
	defer c.Close(context.Background())
	if _, err := c.Initialize(t); err != nil {
		return s, err
	}
	res, err := c.CallTool(t, "conversation_create", map[string]any{"datapack_id": s.convDP, "title": "before upgrade"})
	if err != nil {
		return s, err
	}
	if res.IsError {
		return s, fmt.Errorf("conversation_create before the upgrade: %s", res.Text())
	}
	return s, nil
}

func checkData(t *core.T, api *mekoapi.Client, stack k8senv.Stack, s seeded) error {
	list, err := api.ListDatapacks(t)
	if err != nil {
		return err
	}
	have := map[string]string{}
	for _, d := range list {
		have[d.ID] = d.Name
	}
	var missing []string
	for id, name := range s.datapacks {
		if have[id] != name {
			missing = append(missing, fmt.Sprintf("%s (%s, listed as %q)", id, name, have[id]))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("datapacks from before the upgrade are gone or renamed: %v", missing)
	}
	agents, err := api.ListAgents(t)
	if err != nil {
		return err
	}
	found := false
	for _, a := range agents {
		found = found || a.AgentID == s.agent
	}
	if !found {
		return fmt.Errorf("agent %s from before the upgrade is gone", s.agent)
	}
	without, err := stack.DatapacksWithoutLangfuseProject(t)
	if err != nil {
		return err
	}
	for _, id := range without {
		if _, ok := s.datapacks[id]; ok {
			return fmt.Errorf("datapack %s lost its Langfuse project in the upgrade", id)
		}
	}
	return nil
}

// poller reads the API through its Service every second while the upgrade
// rolls, and keeps the longest stretch in which every read failed.
type poller struct {
	stack   k8senv.Stack
	done    chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	failed  int
	total   int
	downAt  time.Time
	lastOK  time.Time
	longest time.Duration
}

func startPoller(stack k8senv.Stack) *poller {
	p := &poller{stack: stack, done: make(chan struct{})}
	p.wg.Add(1)
	go p.loop()
	return p
}

func (p *poller) loop() {
	defer p.wg.Done()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		err := p.stack.ProbeAPI(ctx)
		cancel()
		p.record(err == nil, time.Now())
	}
}

func (p *poller) record(ok bool, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total++
	if ok {
		p.downAt, p.lastOK = time.Time{}, at
		return
	}
	p.failed++
	if p.downAt.IsZero() {
		p.downAt = at
		if !p.lastOK.IsZero() {
			p.downAt = p.lastOK
		}
	}
	if d := at.Sub(p.downAt); d > p.longest {
		p.longest = d
	}
}

func (p *poller) stop() {
	close(p.done)
	p.wg.Wait()
}

func (p *poller) result() (time.Duration, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.longest, p.failed, p.total
}
