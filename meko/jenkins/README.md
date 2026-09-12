# meko image build

Builds every meko component image from its source repo and pushes it to
`quay.io/yugabyte/meko-*`, in parallel on one ephemeral AWS instance per image
— except the langfuse pair, which shares one instance because the worker mounts
most of the web image's layers. Nothing is built on the Jenkins agent: it resolves refs with
`git ls-remote`, launches the builders, and terminates them on exit. Same shape as
amp's build (`jenkins/amp/` in yugabyte/meko-devops).

| Component | Source repo | Built from | Image |
|---|---|---|---|
| `api-server` | `yugabyte/meko` | `meko/api_server/build-image.sh` | `meko-api-server` |
| `ui` | `yugabyte/meko` | `meko/meko_ui/Dockerfile` | `meko-ui` |
| `inference-gateway` | `yugabyte/meko` | `meko/inference_gateway/Dockerfile` | `meko-inference-gateway` |
| `provisioner` | `yugabyte/meko` | `make docker-build` | `meko-provisioner` |
| `mcp-server` | `yugabyte/meko-mcp-server` | `Dockerfile` | `meko-mcp-server` |
| `rag-agent` | `yugabyte/yugabyte-db` | `python/ai/rag_agent/Dockerfile` | `meko-rag-agent` |
| `langfuse-web` | `yugabyte/meko-langfuse` | `web/Dockerfile` | `meko-langfuse-web` |
| `langfuse-worker` | `yugabyte/meko-langfuse` | `worker/Dockerfile` | `meko-langfuse-worker` |

Where a component already has a build script or Make target, the pipeline calls it
instead of restating the build.

## Versioning

Same logic as `jenkins/amp`, anchored on the meko repo. `MODE=release` finds the last
`meko-0.*` git tag, checks whether product code changed under `meko/` since it (a
denylist — everything is product unless it is a test or a doc), computes
`0.<commit date>.<patch>` from the tags already cut that day, publishes every image at
`<version>-<short-sha>`, and pushes the `meko-<version>` git tag. `MODE=custom` tags
`custom-<short-sha>`. `auto` picks release for a parameterless main build, custom
otherwise. One version covers all eight images; the per-repo shas are recorded in
`build-manifest.txt`.

Changes in meko-mcp-server, meko-langfuse or yugabyte-db do not trip the release gate —
pass `MODE=release` explicitly to publish those.

A tag already on quay is skipped, so a rerun after a partial failure only builds what is
missing.

No cron yet — every build is manual.

## Jenkins job

Runs on the `amp-agent` pool, in the same `amp-jenkins-agent` container amp's build
uses. `users/shubin/meko-devops/MEKO/meko-build`, Pipeline script from SCM →
`yugabyte/meko`, script path
`meko/jenkins/pipelines/pipeline_meko_build.groovy`. Needs the `quay-mekobot`
credential, `aws-ssh-key` and `jenkins-user-key-vcs` for the builders, and an
AWS credential to launch them. The images a run published are listed in the
archived `meko-build-manifest.txt` — only the ones it actually built and pushed, so
components skipped because the tag was already on quay are absent, and a fully
cached rerun archives nothing.

## Running it locally

```bash
export QUAY_USER='yugabyte+mekobot' QUAY_TOKEN='<token>'
VERSION=custom-abc1234 IMAGES=ui,api-server bash meko/jenkins/scripts/build-images-aws.sh
```

Needs AWS credentials and an ssh-agent holding the `yb-dev-aws-2` key.
