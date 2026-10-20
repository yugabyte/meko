# Triaging a failed run

Every mekoqa run lands on the meko report portal at `https://meko-qa.dev.yugabyte.com`. This page is the
path from a red run to one of two decisions. Either a product bug to file, or a test bug to fix.

The rules:

- A **product bug** gets a GitHub issue and a known issue on the portal. The test keeps failing
  until the product is fixed. Nobody works around it in a test.
- A **test or framework bug** gets fixed in the code. No GitHub issue, no known issue.
- Never edit portal state to make a result look better. Fix the cause and rerun.

The `yb meko` commands change often. Run `yb meko --help` and `yb meko <command> -h` before
first use in a session, and trust them over this page. On some leaf commands, such as
`result get`, `--help` prints the top-level help and `-h` prints the command's.
The `meko-report-service` agent skill, in the repository's root skills directory, covers the
CLI, the MCP server and the web UI in more depth.

## Setup

```bash
curl -fsSL https://raw.githubusercontent.com/yugabyte/qa-services/main/yb-cli/install.sh | bash
yb meko --help
```

Reinstalling resets the stored Jenkins credentials, so install once and update only when
`yb meko` is missing a command. The portal also runs an MCP server with the same tools, over
streamable HTTP at `https://meko-qa.dev.yugabyte.com/mcp` with no auth. Add that URL to your agent's MCP
configuration.

Ids accept a full UUID, a portal URL or the 8-character id the tables print. `--json` returns
a stable envelope. Anything that writes needs `--yes`.

## The path

### 1. Find the run

```bash
yb meko run list --status failed
yb meko run get <run-id-or-url>
yb meko result list --run <run-id> --status failed
```

`run get` shows the branch, commit, image tag, report bucket, parallelism, seed, the test
list with statuses, and the Jenkins build. Note the image tag and the parallelism. Both
matter later.

If the run has no tests, the framework never reached the portal. Open the Jenkins console from
the build URL and look for `portal sink` lines.

### 2. Read the result

```bash
yb meko result get <result-id>
```

The output has the error, the step tree, the metrics and the attachment list. Read in this
order.

1. **Status.** It says where the failure happened.

   | Status | Likely cause | Next |
   |---|---|---|
   | `failed` | an assertion, or the guard saw a service misbehave. A product signal until shown otherwise | read the error and the failing step |
   | `env_failed` | the stack could not be built, or the cluster API was unreachable. Not a product signal | read the provision logs, see below |
   | `timed_out` | the body outran its timeout | find the step that was running when it fired |
   | `broken` | the test body panicked. A harness bug | fix the test |
   | `skipped` | not run, with a reason | check the reason is the one you expect |

2. **The failed step.** The step tree names the phase. A step marked `[FAILED]` carries the
   error, and properties the test set with `h.Set`. The `service_guard` step failing means the
   body may have passed and a service restarted, panicked or served a 5xx during it.
3. **The error message.** Tests put the expected and the observed value in it. A message that
   names an HTTP status, a code or an id is the evidence to quote in an issue.
4. **Metrics.** Load tests record `load.error_rate`, `load.http_5xx` and per-op latency. A
   result with `env.provision_seconds` far over the usual 20 minutes points at a slow or
   contended cluster.

### 3. Read the files

Each attachment prints a `source`. Pass it, not the file name.

```bash
yb meko files read <source> --batch 0 --size 300
yb meko files search-in <source> "panic|Traceback" --regex
yb meko files download-all <result-id>
```

`search-in` matches a literal substring by default, so a `|` alternation returns nothing
unless you pass `--regex`. Where to look:

| Attachment | Look for |
|---|---|
| `Stack logs/meko-api-server-*/meko-api-server.log` | the request that failed, panics, 5xx |
| `Stack logs/meko-api-server-*/migrate.log`, `.../ensure-extensions.log` | a stack that never came up (`Dirty database version`) |
| `Stack logs/meko-mcp-server-*/meko-mcp.log`, `inference-gateway-*/gateway.log` | tracebacks on MCP and chat paths |
| `Stack logs/langfuse-web-worker-*/` | Langfuse schema init and project provisioning |
| `Stack events.txt`, `Stack resources/pods.txt`, `Stack describe/` | Pending, OOMKilled, ImagePullBackOff, restarts |
| `Stack llmmock calls.json` | exactly what the product sent to the model and what answered |
| `Stack values.yaml` | the Helm values the stack was built from, secrets masked |
| `Universe logs/` | the database pods, for a stack that lost its database |
| `mekoload-*/clientpod.log`, `*-summary.json`, `*-events.json` | the load clients' own view, per-op errors and invariant breaks |

Collection is `on_failure` by default, so a green test has none of these. A previous-instance
log (`<container>.previous.log` beside the current one in `Stack logs/<pod>/`) shows why a
container restarted.

### 4. Check history

```bash
yb meko result history <result-id> --limit 20
```

A test that was green on the previous build and red on this one points at a change in that
range. A test that flips between runs is flaky, and a flaky test is a bug to find. Compare the
build tags to see which meko build the change entered in.

Two things to rule out before you blame the product:

- **Parallel interference.** On a shared stack up to four tests run together. If a result fails
  at `--parallel 4` and passes alone, rerun it alone (`--parallel 1`, or
  `-o env.stack=dedicated`) and say so in the issue. A failure that needs contention is still
  real, but it is a different bug from one that fails alone.
- **A rerun that overwrote the evidence.** A rerun's results land on the original run as
  further attempts, and the latest attempt is what the run page shows. `result list` hides
  superseded attempts. Add `--include-superseded` before you triage a wall of `env_failed`
  that a rerun may have replaced.

### 5. Decide: product or test

Work through these questions and write down the answer to each.

| Question | If yes |
|---|---|
| Does the error name a product response (a status, a code, a body) that contradicts documented or tested behaviour? | product |
| Does a stack log show a panic, traceback, restart or 5xx at the time of the failure? | product |
| Does the test fail the same way alone, on a dedicated stack, and on the previous release? | an old bug, not a regression. Product if the other answers say so |
| Did the test ask for something the product never promised (a route that changed, an assumption about ordering or timing)? | test |
| Did a harness step fail (provision, clean-up, a mock call, `kubectl`) before the product was exercised? | test or environment |
| Is the status `env_failed`, `broken` or a provision timeout? | not product |
| Does the same failure show up in unrelated tests at the same time? | the stack or the cluster |

If the evidence does not settle it, say so in the notes and keep the claim as an inference.
Reproduce before you call it a product bug: rerun the one test, as in
[Reproducing locally](#reproducing-locally).

### 6. Act

**Product bug.**

1. Search for an existing issue first. The same symptom may be known under another name.

   ```bash
   yb meko issue list --search "no available universe"
   gh issue list -R yugabyte/meko --search "no available universe"
   ```

2. If one exists, attach it to the failed results:

   ```bash
   yb meko issue attach <issue-id> --result <result-id> --yes
   ```

3. If none exists, file a GitHub issue on `yugabyte/meko` (or the component's own repo) with
   the run URL, the result id, the image tag, the exact request and response, the stack log
   lines, and whether it reproduces alone. Label what you observed and what you infer
   separately. Then link it on the portal:

   ```bash
   yb meko issue create --url https://github.com/yugabyte/meko/issues/<n> --result <result-id> --yes
   ```

   `issue create` is idempotent on the URL, so it is safe to script.

The failure now counts as triaged. The test stays red until the product is fixed, and that is
correct. `yb meko status` shows the per-release split between known and untriaged failures.

**Test or framework bug.**

1. Fix the test, the app or the framework in `meko/test/functional`. See
   [`WRITING_TESTS.md`](WRITING_TESTS.md).
2. Replay only the failed tests under the original run's conditions:

   ```bash
   yb meko run rerun <run-id> --result <result-id> --yes
   ```

   A rerun checks out the original branch and commit unless you override `--branch`, so pass the
   branch that carries your fix. A passing attempt clears the failure on the original run.
3. Keep going until the run is green or the only red results have a known product issue.

**Environment failure.** Read `Stack events.txt`, `Stack resources/pods.txt` and the provision
logs. Typical causes are a node pool at its CPU ceiling, an image the registry does not have, a
quota, and a leaked namespace from a killed run. A leak is reaped by `meko-cleaner` or
`mekoqa env reap --plan`. Rerun once the cause is gone. A second `env_failed` for the same
reason is a harness or cluster problem to fix, not a flake to rerun.

## Reproducing locally

Rerun the one test with the failing run's image tag and options:

```bash
go run ./framework/cmd/mekoqa run --test <name> --image-version <tag> \
  --kube-context <ctx> --seed <seed> \
  -o secrets.quay=env:MEKOQA_QUAY -o <the options of the failed run> --portal ""
```

Add `--env-mode keep-failed` only when the user asked to keep the stack, and remove it with
`go run ./framework/cmd/mekoqa env rm <id>` when the investigation ends.
The seed is on the run page. The `-o` options of a portal-triggered run are in the parameters
of its Jenkins build (`OPTIONS`), and the portal's `default_options` apply on top. See [`RUNNING.md`](RUNNING.md) for the details and for
attaching later runs to the kept stack with `--env <id>`.

## Perf and known issues

```bash
yb meko perf -h            # datapoint history and regression comparisons
yb meko issue get <issue-id>   # everything one known issue explains
```

A perf result compares each datapoint to a trailing baseline. Two results compare only when
their `load_signature` matches, so a changed ladder or pod count starts a new baseline and is not
a regression. Do not record or accept a latency from a level that had errors.

## Anti-patterns

- Closing a failure as flaky without the history that shows it flaked.
- Filing a GitHub issue for a test defect, or fixing a product defect inside a test.
- Retrying, sleeping or loosening an assertion until a product bug stops showing.
- Skipping a test to get a green run. A skipped test hides the bug and counts as nothing.
- Reading only the error line. The step tree, the stack logs and the history decide it.
- Guessing flags from memory, or calling the portal with `curl` for what the CLI or MCP does.
- Triaging a run's `env_failed` wall without checking for superseded attempts.
- Triggering runs or changing portal defaults to test the tooling. Use `--dry-run`.
