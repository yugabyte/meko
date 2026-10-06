// Package loadtest is what the stress and perf suites share: minting the
// credentials load clients use, starting meko-load pods next to the stack, and
// judging what the load did to it.
package loadtest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	k8senv "github.com/yugabyte/meko/meko/test/functional/framework/env/k8s"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

const (
	// MaxActivePATs is the api-server's cap per account. Every credential the
	// rig mints and every PAT a pat_churn run holds counts against it.
	MaxActivePATs = 10
	// patHeadroom keeps room for the tokens the coordinator itself mints.
	patHeadroom = 2
	// RunPrefix starts the name of everything meko-load creates.
	RunPrefix = "mekoload-"
)

// Plan is the shape of the client side of a test.
type Plan struct {
	Pods        int
	Credentials int
	// PATBudget is how many PATs one pod's pat_churn may hold at once, 0 for a
	// test that runs none.
	PATBudget int
}

// Check refuses a plan whose tokens would not fit under the account's cap.
func (p Plan) Check() error {
	if p.Pods < 1 || p.Credentials < 1 {
		return fmt.Errorf("a plan needs at least one pod and one credential, got %d and %d", p.Pods, p.Credentials)
	}
	if need := p.Credentials + p.Pods*p.PATBudget; need > MaxActivePATs-patHeadroom {
		return fmt.Errorf("%d credentials plus %d pods holding %d PATs each is %d active PATs, and the account allows %d with %d kept free",
			p.Credentials, p.Pods, p.PATBudget, need, MaxActivePATs, patHeadroom)
	}
	return nil
}

// Rig is the load clients of one test.
type Rig struct {
	T     *core.T
	Env   *k8senv.Env
	Stack k8senv.Stack
	API   *mekoapi.Client
	Fleet *mekoload.Fleet
	Plan  Plan

	tokens []mekoapi.Token
	logf   func(string, ...any)
	// drift is counted minus datapacks when the rig started.
	drift    int
	hasDrift bool
}

type universeUsage interface {
	UniverseUsage(ctx context.Context) (mekostack.UniverseUsage, error)
}

// UniverseCounter checks the api-server's universe counter against the
// datapacks table. Each create takes a slot and each delete gives it back, so
// the difference must be what it was before the load. A drift means a create
// or delete took or lost a slot without a datapack to show for it, and enough
// of them make every create answer 500 with no universe with capacity.
func (r *Rig) UniverseCounter(ctx context.Context) error {
	src, ok := r.Stack.(universeUsage)
	if !ok {
		return nil
	}
	u, err := src.UniverseUsage(ctx)
	if err != nil {
		return err
	}
	r.logf("loadtest: universe counter %d of %d, %d datapacks", u.Counted, u.Max, u.Datapacks)
	if !r.hasDrift {
		r.drift, r.hasDrift = u.Counted-u.Datapacks, true
		return nil
	}
	if d := u.Counted - u.Datapacks; d != r.drift {
		return fmt.Errorf("the universe counter says %d datapacks (of %d allowed) and the table holds %d, %d more than before the load: creates or deletes took or lost slots",
			u.Counted, u.Max, u.Datapacks, d-r.drift)
	}
	return nil
}

// StackConfig is the BackingConfig of a stack under load: env.mekostack.load_api_config added to
// the api-server's api_config.
func StackConfig(cfg *core.Config) map[string]string {
	extra := cfg.GetStringSlice("env.mekostack.load_api_config", nil)
	if len(extra) == 0 {
		return nil
	}
	all := append(cfg.GetStringSlice("env.mekostack.api_config", nil), extra...)
	return map[string]string{"env.mekostack.api_config": "list:" + strings.Join(all, ",")}
}

// Logf is the rig's masked logger.
func (r *Rig) Logf(format string, a ...any) { r.logf(format, a...) }

// Start mints the credentials, starts the pods and points each at the stack's
// in-cluster Services.
func Start(t *core.T, p Plan) (*Rig, error) {
	if err := p.Check(); err != nil {
		return nil, err
	}
	e := stacktest.ClusterEnv(t)
	r := &Rig{T: t, Env: e, Stack: e.Stack(), API: stacktest.API(t), Plan: p, logf: t.Masker().Logf(log.Printf)}
	err := t.Step("start_clients", func(h *core.StepHandle) error {
		for i := 0; i < p.Credentials; i++ {
			tok, err := r.API.CreateToken(t, mekoapi.CreateTokenRequest{Name: fmt.Sprintf("mekoqa-load-%s-%d", t.ResourceSuffix(), i), TTLDays: 1})
			if err != nil {
				return fmt.Errorf("mint credential %d: %w", i, err)
			}
			r.tokens = append(r.tokens, tok)
		}
		h.Set("credentials", len(r.tokens))

		cfg, err := FleetConfig(t.Options)
		if err != nil {
			return err
		}
		r.Fleet = mekoload.New(cfg, r.Stack.Kube(), r.logf)
		if err := r.Fleet.Start(t, "mekoload-"+t.ResourceSuffix(), p.Pods); err != nil {
			return err
		}
		h.Set("pods", r.Fleet.Len())
		h.Set("binary", r.Fleet.Origin().Kind+" "+r.Fleet.Origin().Ref)

		api, mcp := mekostack.ClusterURLs(r.Stack.Namespace())
		return r.Fleet.CreateTargets(t, func(i int) mekoload.TargetSpec {
			return mekoload.TargetSpec{Name: fmt.Sprintf("stack-%d", i), APIURL: api, MCPURL: mcp, GatewayURL: mekostack.ClusterGatewayURL(r.Stack.Namespace()),
				Credentials: r.credentialsFor(i), Timeout: "60s"}
		})
	})
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := r.UniverseCounter(t); err != nil {
		r.logf("loadtest: baseline universe counter: %v", err)
	}
	return r, nil
}

// credentialsFor rotates the list per pod, so the first credentials, which the
// first virtual users take, are not the same on every pod.
func (r *Rig) credentialsFor(pod int) []mekoload.Credential {
	var session string
	if r.Stack.AuthMode() == mekostack.AuthCognitoMock {
		session = r.API.Token()
	}
	out := make([]mekoload.Credential, len(r.tokens))
	for i := range r.tokens {
		tok := r.tokens[(i+pod)%len(r.tokens)]
		out[i] = mekoload.Credential{Name: tok.Record.Name, Token: tok.Value, Session: session}
	}
	return out
}

// FleetConfig reads apps.mekoload.*, with the GitHub token from the secret that
// apps.mekoload.github_secret names. A missing secret is not an error here,
// because a local binary or a source checkout needs none.
func FleetConfig(cfg *core.Config) (mekoload.Config, error) {
	token := ""
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return mekoload.Config{}, err
	}
	if name := cfg.GetString("apps.mekoload.github_secret", "github"); name != "" {
		v, ok, err := secrets.Get(name)
		if err != nil {
			return mekoload.Config{}, err
		}
		if ok {
			token = strings.TrimSpace(v)
		}
	}
	return mekoload.FromConfig(cfg, token)
}

// Launch starts workload on every pod, with config cfg(i) for pod i on top of
// the cleanup and forwarded_for settings every run shares.
func (r *Rig) Launch(workload, forwardedFor string, cfg func(i int) map[string]any) (*mekoload.Load, error) {
	return r.Fleet.StartRun(r.T, workload, func(i int) map[string]any {
		c := map[string]any{}
		if forwardedFor != "" {
			c["forwarded_for"] = forwardedFor
		}
		for k, v := range cfg(i) {
			c[k] = v
		}
		return c
	}, map[string]string{"test": r.T.ResourceSuffix()})
}

// Close attaches what the pods saw, deletes them and revokes the credentials.
// It runs to the end whatever failed before.
func (r *Rig) Close() {
	ctx, cancel := stacktest.CleanupCtx()
	defer cancel()
	if r.Fleet != nil {
		if err := r.Fleet.Close(ctx, sink{r.T}); err != nil {
			r.logf("loadtest: close the fleet: %v", err)
		}
	}
	for _, tok := range r.tokens {
		if err := r.API.RevokeToken(ctx, tok.Record.ID); err != nil && !mekoapi.IsNotFound(err) {
			r.logf("loadtest: revoke %s: %v", tok.Record.Name, err)
		}
	}
	r.tokens = nil
}

type sink struct{ t *core.T }

func (s sink) Add(name, kind string, content []byte) error {
	s.t.AttachBytes(name, kind, content)
	return nil
}

// Sink is an artifact sink onto the test's result.
func Sink(t *core.T) interface {
	Add(name, kind string, content []byte) error
} {
	return sink{t}
}

// Bounds is what a load run may do to the stack and still pass.
type Bounds struct {
	MaxErrorRate float64
	// Max5xx is how many 5xx answers the load may have received.
	Max5xx int64
}

// Judge lists what is wrong with a finished run: pods that failed, an error
// rate or a 5xx count over the bounds, invariants that broke, and a cleanup
// that left something behind. The explanation names the worst error classes.
func Judge(agg mekoload.Aggregate, b Bounds) []string {
	var out []string
	out = append(out, agg.Failed()...)
	if agg.Total.Count == 0 {
		out = append(out, "the load made no calls")
	}
	if agg.Total.ErrorRate > b.MaxErrorRate {
		out = append(out, fmt.Sprintf("error rate %.2f%% is over the bound of %.2f%% (%d of %d calls): %s",
			agg.Total.ErrorRate*100, b.MaxErrorRate*100, agg.Total.Errors, agg.Total.Count, Classes(agg)))
	}
	if agg.Http5xx > b.Max5xx {
		out = append(out, fmt.Sprintf("%d calls answered 5xx, %d allowed: %s", agg.Http5xx, b.Max5xx, FiveXXOps(agg)))
	}
	for _, name := range sortedKeys(agg.Invariants) {
		inv := agg.Invariants[name]
		if inv.Violated > 0 {
			out = append(out, fmt.Sprintf("invariant %s broke %d times in %d checks: %s", name, inv.Violated, inv.Checked, strings.Join(inv.Samples, " | ")))
		}
	}
	for i, c := range agg.Cleanups() {
		if !c.Clean() {
			out = append(out, fmt.Sprintf("cleanup of pod %d left datapacks %v, PATs %v, errors %v", i, c.LeakedDatapacks, c.LeakedPATs, c.Errors))
		}
	}
	return out
}

// Classes renders the error classes of a run, biggest first, with a sample message.
func Classes(agg mekoload.Aggregate) string {
	type row struct {
		op, class string
		n         int64
	}
	var rows []row
	for op, o := range agg.Ops {
		for c, n := range o.ErrorsByClass {
			rows = append(rows, row{op, c, n})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].op+rows[i].class < rows[j].op+rows[j].class
	})
	var parts []string
	for i, r := range rows {
		if i == 5 {
			break
		}
		s := fmt.Sprintf("%s %s x%d", r.op, r.class, r.n)
		if sm := agg.ErrorSamples[r.op+"/"+r.class]; len(sm) > 0 {
			s += " (" + sm[0] + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// FiveXXOps names the ops that got a 5xx and which status.
func FiveXXOps(agg mekoload.Aggregate) string {
	var parts []string
	for _, op := range agg.OpNames() {
		for code, n := range agg.Ops[op].Statuses {
			if c, _ := strconv.Atoi(code); c >= 500 {
				parts = append(parts, fmt.Sprintf("%s HTTP %s x%d", op, code, n))
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Survivors checks the stack while the load has stopped and its cleanup has
// not run: every datapack the load still holds is on the server and has a
// Langfuse project, and no datapack of the stack lacks one.
func (r *Rig) Survivors(ctx context.Context, load *mekoload.Load) error {
	live, err := load.Live(ctx)
	if err != nil {
		return err
	}
	held := map[string]string{}
	for _, l := range live {
		for _, d := range l.Datapacks {
			held[d.ID] = d.Name
		}
	}
	list, err := r.API.ListDatapacks(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, d := range list {
		have[d.ID] = true
	}
	var errs []error
	var lost []string
	for id, name := range held {
		if !have[id] {
			lost = append(lost, name+" ("+id+")")
		}
	}
	if len(lost) > 0 {
		sort.Strings(lost)
		errs = append(errs, fmt.Errorf("%d datapacks the load created and still holds are gone from the server: %s", len(lost), strings.Join(lost, ", ")))
	}
	missing, err := r.Stack.DatapacksWithoutLangfuseProject(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if len(missing) > 0 {
		var ours []string
		for _, id := range missing {
			if name, ok := held[id]; ok {
				ours = append(ours, name+" ("+id+")")
			}
		}
		msg := fmt.Sprintf("%d datapacks have no Langfuse project (MEKO-621)", len(missing))
		if len(ours) > 0 {
			msg += ", of them the load's: " + strings.Join(ours, ", ")
		}
		errs = append(errs, errors.New(msg))
	}
	return errors.Join(errs...)
}

// NothingLeft checks that no datapack of the load is left on the server.
func (r *Rig) NothingLeft(ctx context.Context) error {
	list, err := r.API.ListDatapacks(ctx)
	if err != nil {
		return err
	}
	var left []string
	for _, d := range list {
		if strings.HasPrefix(d.Name, RunPrefix) {
			left = append(left, d.Name)
		}
	}
	if len(left) > 0 {
		sort.Strings(left)
		return fmt.Errorf("%d datapacks of the load remain after its cleanup: %s", len(left), strings.Join(left[:min(len(left), 10)], ", "))
	}
	return nil
}

// IntList reads a list of integers from config.
func IntList(cfg *core.Config, key string, def []int) []int {
	raw := cfg.GetStringSlice(key, nil)
	if raw == nil {
		return def
	}
	out := make([]int, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			panic(fmt.Sprintf("%s: %q is not a positive integer", key, s))
		}
		out = append(out, n)
	}
	return out
}

// Split shares total over n pods, the first pods taking the remainder.
func Split(total, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = total / n
		if i < total%n {
			out[i]++
		}
	}
	return out
}

// Progress returns a Wait callback that logs one line every interval.
func Progress(logf func(string, ...any), label string, every time.Duration) func(mekoload.Aggregate) {
	var last time.Time
	return func(a mekoload.Aggregate) {
		if time.Since(last) < every {
			return
		}
		last = time.Now()
		logf("%s: %.0fs, %d calls, %.1f%% errors, %d 5xx, %d invariant violations, pods %v",
			label, a.Elapsed, a.Total.Count, a.Total.ErrorRate*100, a.Http5xx, a.Violations, a.States)
	}
}
