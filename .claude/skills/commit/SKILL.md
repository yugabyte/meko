---
name: commit
description: Create a git commit or PR in the agent-k-local repo. Use whenever committing, amending, or writing a commit message, PR title or PR description in this repo. Subjects and PR titles use `<type>(<component>[/<area>]): <summary>` so readers see which part of the repo changed and the release workflow can cut releases.
---

# Committing in agent-k-local

## Authorship and footers

Add the footer lines your harness asks for (a `Claude-Session:` or `Co-Authored-By:` trailer,
a session URL at the end of a PR description) exactly as it gives them. Trailers go last in a
commit message, after the body.

In a PR description, the Meko lines AgentK's PR prompt asks for
(`<Meko host>://<datapack id>;<datapack name>` and `meko_conversation_id:<id>`) stay last: put
the harness's lines just above them.

Use the git identity already configured (`git config user.name` / `user.email`). Never
pass `--author` or change the identity.

## Subject format

```
<type>(<component>[/<area>])[!]: <summary>
```

The release workflows read commit subjects. The repo releases two products, each on its own train:
Meko (`meko-release.yml`, tags `meko-v0.YYYYMMDD.N`) and AgentK (`agentk-release.yml`, tags
`agentk-v0.YYYYMMDD.N`). A push to `main` with any `feat`, `fix`, `perf` or `deps` commit since a
product's last release cuts its next version, and those subjects become its release notes (and
AgentK's **What's new**). Subjects with component `meko` release Meko; every other component
(`agentk`, `repo`) releases AgentK (`scripts/release/release-notes.sh --product`). Every other type
lands without a release. Use exactly one of these nine types:

| Type | Releases? | Use for |
|---|---|---|
| `feat` | yes | New user-visible behavior |
| `fix` | yes | Bug fixes, including build or packaging fixes that change what ships |
| `perf` | yes | Faster, same behavior |
| `deps` | yes | Dependency bumps that ship (Dependabot's npm and Go PRs use it too: `deps(agentk)`, `deps(meko)`, `deps(meko/memory-engine)`) |
| `ci` | no | Workflows, Dependabot config, GitHub Actions bumps |
| `test` | no | Tests only |
| `docs` | no | Markdown docs: README, CONTRIBUTING, AGENTS, REVIEW, `docs/` |
| `nit` | no | Comment-only changes, typos, formatting, pure renames or refactors with no behavior change |
| `chore` | no | Anything else that doesn't change what ships |

There is no `refactor`, `build`, `style` or `revert`:

- A refactor with no behavior change is `nit` (or `chore` when it's large).
- A build or packaging change is `fix` when it changes the release, `chore` otherwise.
- A revert takes the type of what it does for users: reverting a broken `feat` is a `fix`.
  Rename GitHub's default `Revert "…"` title.
- A change that only touches code comments is `nit`, even in a file that ships: it must not cut
  a release.

The rest of the subject:

- `(<component>)` is required: the part of the repo the change is in, one of

  | Component | Where |
  |---|---|
  | `agentk` | AgentK, `extensions/agentk` |
  | `meko` | Meko, `backend/` |
  | `repo` | The shared root: `.github/`, `scripts/`, root docs, `.claude/` |

  A change that spans two takes the one it's mainly for. A root file that belongs to one
  component (`agentk-release.yml` builds AgentK) takes that component, not `repo`. The list is
  `COMPONENTS` in `scripts/check-title.sh`; a new extension adds its name there.
- `/<area>` is optional: one lowercase name (letters, digits, dashes) for the part of the
  component it touches, e.g. `agentk/web`, `agentk/server`, `agentk/meko` (AgentK's Meko
  integration), `agentk/update`, `meko/store`.
- `!` after the component marks a breaking change (`feat(agentk)!:`, `fix(agentk/web)!:`), or add a
  `BREAKING CHANGE:` footer. Only `feat`, `fix`, `perf` and `deps` can be breaking: a breaking
  change ships, so it needs a releasing type.
- Summary: imperative mood, lowercase except proper nouns (Homebrew, Meko), no trailing period
  or space. The whole subject is ≤ 72 characters, not counting a ` (#123)` PR number.
- Body (optional): explain *why*, wrapped at 72 columns.

Check a subject before using it; it prints what's wrong and the rules:

```sh
sh scripts/check-title.sh "fix(agentk/host): resume sessions after a host reboot"
```

Example:

```
fix(agentk/host): resume sessions with claude --resume after a reboot

The server now reconciles state.db against the session host on boot and
relaunches any session whose pane is gone, so a devserver crash no longer
loses open work.
```

## PR titles

PRs are squash-merged with the PR title as the commit subject on `main`, so **the PR title is
the subject the release workflow reads**. Give it the same format: pick the type for the PR's
change as a whole (a PR that adds a feature and its docs and tests is `feat`), and run
`sh scripts/check-title.sh "<title>"` before `gh pr create --title` or `gh pr edit --title`.
When later commits change what a PR does, retitle it to match. The `PR title` check
(`.github/workflows/pr-title.yml`) rejects a title the script rejects, but picking the right
type is up to you, and it's what keeps a fix from missing its release.

## Steps

1. `git status` and `git diff --staged` (and unstaged `git diff`) to see what's changing.
2. Stage only the files that belong to this change. Never stage secrets, `.env`, `state.db`,
   certificates, or anything under `~/.agent-k-local`.
3. Write the message per the rules above and check its subject with
   `sh scripts/check-title.sh "<subject>"`.
4. Commit with a heredoc so formatting is preserved: `git commit -F- <<'EOF' ... EOF`.
