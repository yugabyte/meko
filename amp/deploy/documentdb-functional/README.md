# DocumentDB functional-test gate

Up to 0.116-0, upstream documentdb gated its own PRs on an allowlist of about
10,500 functional tests. These are the tests from
[documentdb/functional-tests](https://github.com/documentdb/functional-tests)
that pass reliably on `documentdb-local`. This directory runs that same gate
against an amp documentdb endpoint's gateway, which is the path a customer's
driver or mongosh takes. It is the `/ci amp_deploy_gke_documentdb` suite
([`docs/ci-directives.md`](../../docs/ci-directives.md)).

## What runs

The gate comes from the **0.116-0** release, not from upstream's `main`, while
the endpoint runs the release amp ships (1.0-0). 1.0 replaced the allowlist with
a known-failures model: about 50,000 tests, with failing and flaky lists under
strict xfail. This gate has not been ported to it yet, so it still runs 0.116's
~10,500 tests against the 1.0 gateway. [`run.sh`](run.sh) downloads the
`v0.116-0` source tarball through `scripts/safe_curl.sh`, against its pin in
`downloads_checksum.txt`, and takes three things from
`documentdb-local/functional-tests/`:

- `config/allowlist.yml`;
- `config/image.yml`, the functional-tests image, pinned by digest;
- `tools/functional_gate.py`, upstream's gate tooling.

The three must come from one release: the allowlist's node IDs name tests in the
suite the image pins. `GATE_VERSION` in `run.sh` sets that release.

Against a deployed controller, `run.sh`:

1. creates an `engine:"documentdb"` cluster and an endpoint, and waits for its
   gateway. compute_ctl creates the extension in a `documentdb` database, and
   the gateway binds only once it exists.
2. puts the gate files, [`exclusions.yml`](exclusions.yml),
   [`amp_gate.py`](amp_gate.py) and [`shard.sh`](shard.sh) into a ConfigMap.
   The allowlist is gzipped, since it is larger than a ConfigMap holds.
3. runs `SHARDS` pods of the functional-tests image in the namespace. Each pod
   runs one round-robin slice of the allowlist through `shard.sh` against the
   endpoint's `documentdb_address`, which is the in-cluster Service. They log in
   as the default user over SCRAM-SHA-256. `shard.sh` is upstream's own
   per-shard step: xdist, then a sequential re-run of up to 200 non-passes,
   merged over the parallel report.
4. merges the shard reports and enforces the allowlist over the whole set with
   upstream's `summarize-gate`, as upstream's combine job does. It passes only
   if **every** gated test was collected, ran and passed. The summary goes to
   the job page, and the reports and logs are uploaded as
   `documentdb-functional-results`.

Run it by hand against a namespace where the controller is up and the
documentdb extension and gateway images are pushed:

```bash
NS=<namespace> deploy/documentdb-functional/run.sh     # SHARDS, WORKERS, TIMEOUT_MIN, KEEP=1
```

## Exclusions

[`exclusions.yml`](exclusions.yml) lists allowlisted tests that fail on amp,
each with the reason. [`amp_gate.py`](amp_gate.py) removes them from the
allowlist, and upstream's tooling then runs unchanged on what remains. Rules:

- An entry needs a **reason a reviewer can check**, and should link the issue
  that tracks the gap. An exclusion records a known gap. It is not a waiver.
- An entry goes when its gap closes.
- An entry that no longer names an allowlisted test, because upstream dropped
  or renamed it, **fails the run**. The list cannot outlive what it excludes.

Upstream's `engines:` scoping is honoured, and the engine is `documentdb`.
