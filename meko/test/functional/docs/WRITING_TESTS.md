# Writing a test

Read [`ARCHITECTURE.md`](ARCHITECTURE.md) first for what a stack is and how the scheduler uses
`EnvSpec`. The mandatory rules for every change are in
[`../framework/AGENTS.md`](../framework/AGENTS.md).

## Rules that decide what a test may do

- **Product bugs are reported, never worked around.** If the product misbehaves, the test
  fails with a message that names the behaviour, and you file a GitHub issue. You do not
  add a retry, a sleep, a looser assertion or a skip to make it pass. A comment naming the
  issue (`MEKO-621`) in the failure message is how a reader finds the history.
- **Harness bugs are fixed.** A test that fails because of the test (a wrong route, a race in
  its own setup, a stale assumption) gets fixed in the test code. No issue is filed.
- **LLMs are always mocked.** Tests never call OpenAI or Bedrock. Script the mock instead.
- **Images come only from Quay through `meko-build`.** The framework refuses a `meko-*` image
  from anywhere else.
- **Touch only what you created.** A test names its resources with `t.ResourceSuffix()`,
  deletes them, and never reads or changes another namespace, another test's datapacks or a
  cluster-scoped object.
- **Never decide the verdict from a clock.** Wait for a condition with `t.WaitFor`, not a
  fixed sleep. A test that passes only after a sleep hides the product's real latency.

## Where a test goes

A test is a Go file in one of the three suite packages under `suites/`. Pick the suite by
what the test needs.

| Suite | Put it here when | Typical `EnvSpec` | Category, tags |
|---|---|---|---|
| `functional` | it checks product behaviour on the shared stack: a feature, a fault, an upgrade, a universe | `Shared`, `SingleStack`. Add `FaultsSharedInfra: true` to fault the stack. A test that uses nothing of the shared stack, building its own universes or stack, is `Exclusive`, `ClusterOnly`, so a failed shared stack does not block it. Add `ClusterSlots: 1` to move it between releases | `Functional`, `tags()`, which is `regression` and `nightly`. Pass an area to add one: `tags("upgrade")` |
| `stress` | it loads a stack for a long time | `Exclusive`, `DedicatedStack`, `ClusterSlots: 1` | `Stress`, `stress` |
| `perf` | it records latency and throughput | `Exclusive`, `DedicatedStack`, `ClusterSlots: 1` | `Perf`, `perf` |

`Category` sets the default timeout (functional 10m, stress 60m, perf 150m, long running 24h,
all overridable under `timeouts.` in `default.yaml`). Tags are what `--tag` selects on. Every
`functional` test must carry `regression`, which is what the portal's Functional card runs.
There is no fourth suite.

Inside a suite, files that call `core.Register` keep descriptive names, and pure
support code ends in `_helper.go`. Registered tests are ordinary package code, not `_test.go`.

## Registering

```go
func init() {
	core.Register(core.Test{
		Name:        "functional/pat_sees_datapack",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamMCPServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "A datapack created over REST is listed by datapack_list over MCP, called with a PAT.",
		Run:         patSeesDatapack,
	})
}
```

`Register` panics on an empty or duplicate name. Fields worth knowing:

| Field | Use |
|---|---|
| `Name` | `<suite>/<test>`, unique |
| `OwningTeam` | one of `TeamAPIServer`, `TeamMCPServer`, `TeamInferenceGateway`, `TeamUI`, `TeamPlatform`. Routes triage. Empty only for non-product tests |
| `Description` | one line that says what is asserted. `collect`, `describe` and the portal show it |
| `Params` | config prefixes the test reads (`"db"`, `"stress"`). `mekoqa describe <test>` lists their keys |
| `Timeout` | overrides the category default. Set it when the body is legitimately longer |
| `Versions` | `VersionSpec` bounds. The framework skips outside them |
| `Skip` | a reason the behaviour is not implemented yet. The test reports as skipped and costs no environment. Delete the field when it lands |
| `Preflight` | returns a skip reason from the run config, for a test that needs a credential or opt-in |
| `Tolerates` | guard finding kinds the test provokes on purpose |
| `Unguarded` | skips the guard for a body that never calls the stack's services |
| `Setup` | a separable provision-and-prepare phase for `--setup-only` |

### EnvSpec

`EnvSpec{Sharing, Shape}` picks the environment, as described in the scheduler section of the
architecture doc.

- Use `Shared` plus `SingleStack` by default. The test shares the run's stack with up to four
  others, so it must use only its own resources.
- Use `DedicatedStack` when the test needs a stack nobody else touches: load, a pod kill, a
  database of its own (`EnvSpec.Database`), or stack settings of its own (`BackingConfig`, which
  overrides `db.*` and `env.mekostack.*` keys for that stack only). It takes a whole stack, about
  20 minutes to build, so reach for it only when the test needs it.
- Set `FaultsSharedInfra: true` when a shared-stack test freezes, kills or reconfigures a
  stack component. The scheduler then runs it alone and checks the stack serves again after.
- Set `SharedLock` to a key when two shared tests would clash on one stack-wide resource.
- Set `ClusterSlots` for a test that builds stacks of its own beyond the one the scheduler
  gave it.

### Tolerates and Unguarded

The guard fails a test when a container restarts, a service log shows a panic or traceback,
or the api-server serves a 5xx while the body runs. If your test causes one on purpose, list
its kind. A pod-kill test lists `core.GuardPodRestart` and `core.GuardHTTP5xx`. Do not list a
kind to hide a finding you did not cause. That is a product bug.

`Unguarded: true` is for a body that never talks to the stack, such as one that builds
universes or a stack of its own. Its guard window would only collect what the tests beside it
caused.

## The body

`Run` is `func(t *core.T) error`. A returned error fails the test. `t` is a
`context.Context` that the test timeout cancels, so pass it to every call.

| Call | Does |
|---|---|
| `t.Step(name, fn)` | runs `fn` as a named, timed span. `fn` receives a `*StepHandle`. A returned error marks the step failed. Steps nest with `h.Step` |
| `h.Set(key, val)` | records a property on the step, visible on the portal |
| `t.Metric(key, value, unit)` | records one observability number |
| `t.MetricSeries(m)` | records a time series |
| `t.Datapoint(dp)` | records a perf number compared against a baseline. Set `Direction` (`HigherIsBetter` or `LowerIsBetter`) |
| `t.SetPerfIdentity(id)` | stamps what was measured. Two perf results compare only when `LoadSignature` matches |
| `t.Attach`, `t.AttachBytes` | attaches a file to the result |
| `t.WaitFor(desc, timeout, interval, cond)` | polls `cond` until true or the timeout |
| `t.Fail`, `t.Failf` | marks the test failed and continues |
| `t.FailNow`, `t.Fatalf` | marks it failed and stops |
| `t.Skip`, `t.Skipf` | skips the test with a reason |
| `t.OnTeardown(fn)` | runs `fn` after the body and collect, last registered first, on its own deadline |
| `t.ResourceSuffix()` | a short hex token unique to this execution. Use it in every name |
| `t.Options` | the resolved run config. `GetString`, `GetInt`, `GetBool`, `GetDuration` take a key and a default |

Structure a body as a sequence of steps. A failed step names the failing phase on the portal
and the error message carries the detail. Return on the first failed step unless later steps
are independent.

Clean up with `defer` and a context that is independent of the test's, because a timeout may
be the reason you are cleaning up:

```go
ctx, cancel := stacktest.CleanupCtx()
defer cancel()
_ = api.DeleteDatapack(ctx, dp.ID)
```

### Metrics versus datapoints

A metric is a number for a reader, such as how many calls ran. A datapoint is a number the
portal compares to a trailing baseline, so give it a direction and use it for latency or
throughput. Do not record a datapoint from a run that had errors, because a percentile over
errors measures nothing.

## Helpers: `suites/internal/stacktest`

| Helper | Gives you |
|---|---|
| `stacktest.API(t)` | the stack's REST client. Fails the test when any error names an internal endpoint (MEKO-812) |
| `stacktest.MCP(t, pat)` | an MCP client for a PAT, with the same leak check on tool results |
| `stacktest.Accounts(t, n)` | `n` accounts on the Cognito mock, and a cleanup function. Skips the test on `auth=none` |
| `account.API`, `account.PAT(scopes...)`, `account.MCP(pat.Value)` | a client for the account, a minted PAT (a `mekoapi.Token`), an MCP client for a PAT's value |
| `stacktest.Anonymous(t)` | the REST client with no token |
| `stacktest.Control(t)` | the Cognito mock's control API, to mint tokens the product never would |
| `stacktest.LLM(t)` | the LLM mock client. Skips the test when the stack has none |
| `stacktest.Universes(t)` | extra universes the test builds, removed after it |
| `stacktest.Gateway(t)` | a forwarded URL for the inference gateway, and the function that closes it |
| `stacktest.Env(t)`, `ClusterEnv(t)` | the stack's environment. They skip on a provider with no stack, or a remote deployment with no kubectl access |
| `stacktest.CleanupCtx()` | a 30s context independent of the test's |
| `stacktest.HasLangfuseProject` | whether a datapack has its Langfuse project mapping |

`stacktest.Env` is also how a test reaches the kube client (`Kube()`), the stack handle
(`Stack()`), the namespace and the database. With the fake provider every one of these skips
the test, so `mekoqa run --fake-env` never fails on a test that needs a stack.

`mekoapi` is a hand-written client for the routes tests call. Add a method there when a test
needs a new route, read off `router.go`. The committed `swagger.yaml` is not the source, it
lacks paths. `mekoapi.Client.Raw` sends an arbitrary request and returns the status and body,
which is what a negative test needs. `mekoapi.IsNotFound(err)` and `StatusOf(err)` classify
errors.

### Accounts and Cognito

Run the stack with `-o env.mekostack.auth=cognito_mock` and call `stacktest.Accounts(t, n)`.
Each account has its own row in the stack's database, is lifted off the free tier, and is
named after the test. `account.PAT("read")` mints a scoped PAT and the cleanup function
`Accounts` returned revokes it. Tests that need accounts skip on `auth=none`, so a run without
the Cognito mock silently skips `access_matrix`, `auth_login_flow` and `datapack_sharing`.
Check the skip count.

The product has no token refresh route and no route that removes an accepted member of a
datapack, so do not write tests for either.

### Scripting the LLM mock

Every rule takes a `Match` substring. Put a marker word from `t.ResourceSuffix()` in your own
text so the rule fires only for your test, and remove your rules when you finish:

```go
llm := stacktest.LLM(t)
marker := "m" + t.ResourceSuffix()

id, err := llm.AddScript(t, llmmock.Script{
	Match: marker,
	Reply: `{"memory": [{"id": "0", "text": "The user likes tea."}]}`,
})
if err != nil {
	return err
}
defer llm.RemoveScript(context.Background(), id)

fault, err := llm.AddFault(t, llmmock.Fault{
	Route: llmmock.RouteEmbeddings, Match: marker, Kind: llmmock.KindStatus, Status: 500, Times: 1,
})
if err != nil {
	return err
}
defer llm.RemoveFault(context.Background(), fault)

calls, err := llm.Calls(t, llmmock.Filter{Route: llmmock.RouteEmbeddings, Contains: marker})
```

Use `Calls` to assert what the product sent to the model, and `Answer` on a call to assert what
answered (`script:<id>`, `builtin:<name>`, `fixture`, `hash`). Never call `Reset`, it clears
other tests' rules. The `SetEmbedding` method maps a text to a vector you choose, so a
two-number vector fixes the angle between two texts.

## Asserting product behaviour without masking bugs

- **Assert the contract, not the current output.** Check status codes and error codes the
  API documents (`403 INSUFFICIENT_SCOPE`, `404`), not the exact message text.
- **Read the product, not the test's belief.** When a result surprises you, read the handler
  before changing the assertion.
- **Do not retry a failure the product should not produce.** A retry is right for something
  the product documents as retriable, such as a 429 with `Retry-After`. It is wrong for a 500.
  If you must wait for asynchronous work, wait on a condition with `t.WaitFor` and fail with
  the last observed state.
- **Fail with the evidence.** Put the observed status, body, id and the expected value in the
  error. A reader on the portal sees only the message and the attachments.
- **One test, one reason to fail.** If two behaviours can break independently, use two tests
  or two independent steps, so a known issue on one does not hide the other.
- **Do not skip around a bug.** `Skip` is for a behaviour that is not implemented. A skipped
  test counts as nothing, so a bug hidden behind it never shows up.
- **Clean up only your own data.** Never delete or modify a datapack or token you did not
  create to make an assertion hold.
- **Do not count on ordering.** Another test runs beside yours. Assert on your own datapack by
  id, not on list lengths.

## Timeouts

Three clocks apply.

- The **test timeout** (`Test.Timeout`, else the category default) bounds the body. When it
  fires the body's context is cancelled, the result is `timed_out`, and collect and teardown
  still run.
- The **provision timeout** (`timeouts.provision`, `timeouts.provision_dedicated`, scaled by
  `timeouts.provision_multiplier`) bounds the stack build and does not count against the test.
- Each **call and wait** inside the body needs its own bound. `t.WaitFor` takes one, a REST
  client has a 30s default (`WithTimeout` changes it), and `StepHandle` work should pass `t`
  so the test timeout reaches it.

For a test that is legitimately long, set `Timeout`, and keep an inner bound on every wait so
a hang fails with a message instead of a bare `timed_out`.

## A worked example

This test compiles in the `functional` package. It is shown here and is not registered in the
tree, so it does not run. To try it, put it in `suites/functional/pat_sees_datapack.go`, run
`go vet ./suites/functional`, then `go run ./framework/cmd/mekoqa describe functional/pat_sees_datapack`.

```go
package functional

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func init() {
	core.Register(core.Test{
		Name:        "functional/pat_sees_datapack",
		Suite:       "functional",
		Category:    core.Functional,
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamMCPServer,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "A datapack created over REST is listed by datapack_list over MCP, called with a PAT.",
		Run:         patSeesDatapack,
	})
}

func patSeesDatapack(t *core.T) error {
	api := stacktest.API(t)
	name := "dp-" + t.ResourceSuffix()

	var dp mekoapi.Datapack
	var tok mekoapi.Token
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		if tok.Record.ID != "" {
			_ = api.RevokeToken(ctx, tok.Record.ID)
		}
		if dp.ID != "" {
			_ = api.DeleteDatapack(ctx, dp.ID)
		}
	}()

	if err := t.Step("create_datapack", func(h *core.StepHandle) error {
		var err error
		dp, err = api.CreateDatapack(t, mekoapi.CreateDatapackRequest{Name: name})
		h.Set("datapack_id", dp.ID)
		return err
	}); err != nil {
		return err
	}

	if err := t.Step("mint_pat", func(h *core.StepHandle) error {
		var err error
		tok, err = api.CreateToken(t, mekoapi.CreateTokenRequest{Name: "mekoqa-" + t.ResourceSuffix(), TTLDays: 1})
		return err
	}); err != nil {
		return err
	}

	mcp := stacktest.MCP(t, tok.Value)
	defer func() {
		ctx, cancel := stacktest.CleanupCtx()
		defer cancel()
		mcp.Close(ctx)
	}()
	if _, err := mcp.Initialize(t); err != nil {
		return err
	}

	return t.Step("datapack_list_names_it", func(h *core.StepHandle) error {
		call := func(tool string, args map[string]any) (string, error) {
			res, err := mcp.CallTool(t, tool, args)
			if err != nil {
				return "", err
			}
			if res.IsError {
				return "", fmt.Errorf("%s answered a tool error: %s", tool, res.Text())
			}
			return res.Text(), nil
		}
		out, err := call("conversation_create", map[string]any{})
		if err != nil {
			return err
		}
		var conv struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &conv); err != nil || conv.ID == "" {
			return fmt.Errorf("conversation_create answered %s", out)
		}
		list, err := call("datapack_list", map[string]any{"conversation_id": conv.ID})
		if err != nil {
			return err
		}
		if !strings.Contains(list, name) {
			return fmt.Errorf("datapack_list did not return %q: %s", name, list)
		}
		return nil
	})
}
```

What the example shows:

- the datapack and token names come from `t.ResourceSuffix()`, so a parallel run never clashes
- the clean-up `defer`s use `CleanupCtx`, not `t`
- each phase is a step, so a failure names the phase on the portal
- the failure message holds what was expected and what came back
- `stacktest.API` and `stacktest.MCP` fail the test if any error names an internal endpoint

## Checking a test before you push

From `meko/test/functional`:

```bash
go build ./... && go vet ./... && gofmt -l . && go test -race ./...
go run ./framework/cmd/mekoqa collect --test functional/<name>      # it registers
go test -race ./suites/canary                                     # the engine self-test still passes
go run ./framework/cmd/mekoqa run --test functional/<name> --fake-env   # it skips, not fails
```

`go test` runs only in-process doubles and never touches a cluster. Then run the test on a
real stack, see [`RUNNING.md`](RUNNING.md). Registered counts are asserted in unit tests, for
example `functional_test.go` checks the number of functional tests, so adding a test to an
existing suite means updating that count.
