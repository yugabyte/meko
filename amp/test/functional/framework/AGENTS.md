# ampqa — Framework Principles (MANDATORY)

These are the load-bearing postulates of the `ampqa` test framework. Every change
— by a human or an agent — MUST uphold them. They are not style preferences;
they are the reasons the framework stays versatile, safe, and maintainable. If a
task seems to require breaking one, stop and raise it rather than break it
quietly.

## 1. KISS — small agnostic engine, everything else is atomic apps
- The engine (`core`, `sched`, `exec`, `report`) knows nothing about what is
  being tested. All communication with the product or any external system is an
  **app** or helper under `apps/` — atomic, flat, reusable from any test, task,
  check, or env.
- No adapter layers, no DI containers, no registries beyond a plain name→value
  map. Plain structs and functions. Add an **interface only where two real
  implementations exist** (sinks, env providers, exec transports, chaos actions).
- New capability = a new app / action / checker / sink / task / env provider.
  **Never edit the engine to add a product feature.** Readable beats clever.
- **Suite file naming.** Registered tests are ordinary package code (not
  `_test.go` — that marks Go unit tests). To keep "where the tests are" obvious,
  inside a `suites/<name>/` package: files that call `core.Register` keep plain,
  descriptive names; pure support code (harness, provisioners, config, shared
  helpers) ends in **`_helper.go`**. This is a filename-only marker — do **not**
  split helpers into a sub-package: they hang methods on and share unexported
  types with the test files (e.g. methods on the suite's `flow` struct), so a
  sub-package would force exporting the whole harness, which fails the KISS bar.
  A genuine reusable app still belongs under `apps/`, not a suite `_helper.go`.

## 2. Standalone module — no product-code coupling
- `test/functional` is its own Go module (framework code under `framework/`,
  suites under `suites/`). It imports **nothing** from
  `controller/`, `libs/`, or any other product code, and no product code imports
  it. `go build ./...` must succeed with the rest of the repo absent.
- Talk to the product only **over the wire**: REST, psql, SSH, kubectl/docker.
  The controller client is **generated from the OpenAPI spec**, not imported.

## 3. Parallel by design — from the first line
- Tests are atomic and independent: their own env/project, no shared mutable
  state, no ordering assumptions. `--parallel N` is first-class; sequential is
  just pool size 1, **never a separate code path**.
- No fixed ports, no shared temp paths, no package-level mutable state (except
  mutex-guarded registries). Everything MUST pass `go test -race`. Use
  auto-allocated ephemeral ports (kubectl `:remote`) and `t.TempDir()`.

## 4. Nothing runs on the operator's machine
- The framework only **coordinates**: it issues CLI/API calls to remote
  instances, pods, and clusters. Environments are always remote — even the dev
  loop (docker envs run on a remote docker host over SSH).
- Never buffer logs/metrics unbounded in memory — **stream** everything. This is
  what keeps a CI agent from OOMing.

## 5. Environment lifetime — resources must outlive `Provision`
- The `ctx` passed to `EnvProvider.Provision` bounds the provisioning **work
  only**. Anything that must outlive the call (port-forwards, SSH sessions,
  background processes owned by the Environment) MUST be rooted in
  `context.Background()` and released in `Teardown` (or provider `Close` for
  shared resources). Tying them to the provision ctx kills them the instant
  provisioning returns — a real bug this framework already paid for.
- **Teardown always runs** once provisioning succeeded, on its own independent
  deadline, even on failure or panic. **Collect logs before killing** any
  workload/app (`apps.StopWithCollect`) — losing evidence is not acceptable.

## 6. Configuration — one mechanism, one place for defaults
- YAML by default (HOCON also parsed). Precedence: embedded
  `configs/default.yaml` (layer zero) → `-c` files deep-merged in order → `-jc`
  JSON → typed `-o` overrides (`int:`/`bool:`/`float:`/`list:`/`json_file:`/
  `json_text:`, dotted keys nest).
- **Every default lives in `configs/default.yaml`** — the single documented home
  for every knob. A key the code reads without a documented default there is a
  bug (the defaults registry test guards this). The fully resolved config is
  recorded into every result.

## 7. Secrets — named, masked, never leaked
- Credentials (tokens, cloud keys, SSH keys, service-account JSON) are declared
  by name in the `secrets` config section, supplied as **token** / **file** /
  **env** (env-var name) variants, and resolved by name. Secret values are
  **masked in logs and never recorded into results**. Do not hardcode
  credentials; do not read private key material into result artifacts.

## 8. Infrastructure hygiene — reuse, don't pollute
- On shared cloud accounts / clusters: **create no redundant or orphan
  resources, reuse shared ones, clean up what you create.** YBA providers reuse
  the shared `yb-dev-aws-2` keypair — never let YBA auto-mint a new AWS key.
- Only ever modify or delete resources **the framework created** (namespaces
  labeled `ampqa-*` / `ampqa-managed-by`, tagged instances). Never touch other
  teams' namespaces, workloads, or cluster-scoped objects. Tag and reap.

## 9. Result honesty & reproducibility
- The status taxonomy (`passed` / `failed` / `env_failed` / `broken` /
  `timed_out` / `skipped`) is set by **where** the failure happened: assertion →
  failed; harness panic → broken; provisioning failure → env_failed; watchdog →
  timed_out. Report faithfully.
- Every test execution gets a **seed** driving all randomness (workloads, nemesis
  sequence/timing/targets). It is recorded; `--seed` replays exactly. Chaos and
  workloads must derive their randomness from the seed, never from wall-clock or
  unseeded RNG.

## 10. Version compatibility — one framework, every product version
- **No per-version framework branches.** A test declares min/max bounds
  (`VersionSpec`) and the framework auto-skips out-of-range (reason recorded).
  Cross-version API differences are handled **inside apps** (check the live
  version, take the other route), never with hand-rolled `if version < X` early
  returns in test bodies.

## 11. Dependencies — minimal and proven
- Prefer stdlib. Add a dependency only when genuinely warranted, and only a
  proven, mainstream one (cobra, yaml.v3, pgx, `golang.org/x/...`, official cloud
  SDKs). No obscure or single-maintainer libraries. `kubectl`/`helm`/`docker` are
  shelled binaries (ambient-authed), not linked SDKs.

## 12. Every change is verified and self-tested
- Before declaring any change done: `go build ./...`, `go vet ./...`,
  `gofmt -l .` (empty), `go test -race ./...` (green). Tests use in-process
  doubles only (scripted fake executors, `httptest`) — **nothing real runs in
  unit tests**. The **canary suite** is the framework's self-test and must stay
  green.
- Comments are sparse — only for invariants/constraints the code can't state.
  **No AI attribution anywhere** — not in code, comments, or docs.

---

*The authoritative rationale for each rule lives in the framework architecture
and implementation plan. When in doubt, the rule above wins; when the rule
itself is unclear, ask before diverging.*
