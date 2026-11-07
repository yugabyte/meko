# AGENTS.md: Present

Guide for coding agents working on **Present**, the Meko extension that builds interactive,
branching presentations from datapacks. The repo's own rules are in the root
[AGENTS.md](../../../AGENTS.md); the design as built is [docs/design.md](docs/design.md). Run the
commands below from `src/extensions/present`.

Present is a server on a unix socket in its home, `<Meko's data folder>/extensions/present`.
Meko shows its pages at `/ext/present/` on its one port, with Meko's access check, as it shows
AgentK at `/ext/agentk/`. The server runs Claude Code headless for each deck (one persisted session
per deck), keeps each deck's draft, saves decks to their Meko datapacks in pieces, and holds the
rooms of live talks.

## Commands

```sh
cd src/extensions/present
nvm use 22
npm ci
npm run typecheck && npm run lint && npm test   # run all three before every commit
npm run build                                   # dist/server + dist/web
npm run dev                                     # server on .dev-home/run/web.sock, Vite on 127.0.0.1:5179
# a release tarball, as present-release.yml builds one (after npm prune --omit=dev; npm ci undoes it):
sh ../../../scripts/release/fetch-node.sh 22.18.0 /tmp linux-x64   # prints the unpacked Node
sh scripts/package-release.sh <version> linux x64 <unpacked nodejs.org Node> out
sh scripts/smoke-release.sh out/present-<version>-linux-x64.tar.gz <version>
```

Under `npm run dev` the pages are at `http://127.0.0.1:5179/` with no `<base>`; Meko's web
listener isn't involved, but the server still asks `meko token --json` which Meko to use: a Meko
on this computer (`meko start`), and through it the Meko Cloud link it keeps (`meko cloud-link`). To try it behind Meko: `npm run build`, put `dist/server/cli.js` on PATH as
`me_present` (`npm link`: only for Meko to find a build from source; people run `meko present
<command>`), and run `meko extension install present`, then `meko ui` (Present is in its sidebar).

## Map

| Path | What it is |
|---|---|
| `src/shared/deck.ts` | The deck: scenes, edges, slides, blocks, assets; `deckProblems`/`checkDeck`, the one set of rules (graph, shapes, size limits) every edit passes. |
| `src/shared/ops.ts` | Every change to a deck as typed operations; `applyOps` (checked, never mutates its input), `describeOp` and `defaultTicks` for change sets. |
| `src/shared/run.ts` | A live run's position and how each console action moves it (`navigate`, `fitRun`). |
| `src/shared/manifest.ts` | The stored form: the `deck.json` manifest, scene pieces, 4 MiB parts, and turning a deck into them and back. |
| `src/shared/datapacks.ts` | Present's datapack prefixes (`present/template/`, `present/anim/`, `present/deck/`) and deck datapack names. |
| `src/shared/types.ts` | The API contract: every route's request and reply and every WebSocket message. Change it first. |
| `src/server/cli.ts` | The `me_present` program, which Meko runs (people run it as `meko present <command>`; the extension is `present`: its folder, tags, tarballs and `/ext/present/`): `serve`, and what Meko runs (`start`, `stop`, `status`, `install`, `uninstall`, `_handoff`, and `_busy`, which exits 0 while a talk is live so Meko's automatic update waits). The pid file counts only when its process is a Present server (`isServer`). Run by `meko start` and `meko stop` (`MEKO_SUMMARY=1`), `start` and `stop` print one row of Meko's summary, and run by `meko status` so, `status` prints only its headline. |
| `src/server/status.ts` | `meko present status`'s output (`statusLines`), laid out as `meko status`: the server and Claude Code rows, a `●` headline, the pages and the home as plain rows, and `Next:`; and `headline`, the one line always marked ✓ or ✗ that `status` prints alone under `MEKO_SUMMARY=1`, which Meko shows in its own row without the mark and the title, and first off a terminal (a Meko from before `MEKO_SUMMARY` on status reads the first line, and would take a row's mark for Present's). The uptime from the pid file's time. |
| `src/server/summary.ts` | That row: Meko's format (`  ✓ Present       started`), colored (bold yellow for `!`) only on a terminal without `NO_COLOR` and with `TERM` not `dumb` (Meko's `docs/install.md`, "The summary rows"). |
| `src/server/server.ts` | Fastify on the socket: pages (`<base href="/ext/present/">`, no framing), API, WebSockets, the same-origin check, `POST /_meko/changed` (Meko's notice that its Meko Cloud link changed, or that its server started) and `GET /_meko/busy` (whether a talk is live, for `_busy`). |
| `src/server/paths.ts` | The home by Meko's rule (`resolve-cases.json`), the socket path (under `/tmp` for a long home, not `$TMPDIR`), `refuseOtherMeko`. |
| `src/server/meko.ts` | `MekoPort` and its MCP + REST client (`datapack_create` never sent twice, `mayRetry`). Which Meko is Meko's to say, from `meko token --json` and never stored (`probeMeko`, `mekoFinder`): always the Meko on this computer, which passes calls on to Meko Cloud while it links it (Meko Cloud named by the link's `web_url`): REST at its `/api/v1`, serving its own datapacks too (`localIds`), or Meko Cloud itself when an older Meko hands out the link's key (REST on the link's own `api_url`, another host); a link Meko couldn't read keeps the last one, or finds no Meko before one was read. One connection at a time; `current()` (Claude's runs) reads Meko afresh. `reset()` (forgetting the link too) when Meko posts `/_meko/changed` (a link change, or its server's start). |
| `src/server/decks.ts` | Drafts, undo and redo, assets, save, load, versions, restore. Each draft names the Meko its datapack is in (`mine`): one of another Meko can be played but not edited, saved or given to Claude. While the Meko on this computer links Meko Cloud, it serves its own datapacks' decks beside Meko Cloud's (`serves`, `localIds`), tagging and retagging them as its own. |
| `src/server/pieces.ts` | Hashing, uploads (four at a time, manifest last, only what the datapack lacks), checked fetches, the cache. |
| `src/server/claude.ts`, `jobs.ts`, `prompts.ts` | `claude -p` with the deck's session; the per-deck job queue (map, build on forks, ask); what Claude is told and must answer. |
| `src/server/runs.ts` | Live run rooms, kept on disk so a restart resumes; they follow every change the deck store makes (`DeckStore.listen`), and a run's moves apply one at a time. `live()`: a run has a window open, or moved in the last 5 minutes. |
| `src/web/` | The pages (React, Vite, Tailwind with Meko's tokens): decks, new deck, map (React Flow), slide editor, console, audience. |
| `packaging/me_present` | The release's launcher, `bin/me_present`: runs the bundled Node (`node/bin/me_present`, so the process is named `me_present`), exports `PRESENT_APP_DIR` and `PRESENT_LAUNCHER`, and hands `serve` to Meko's `meko _launch`, which counts a new version's starts, rolls back one that fails three times and finishes the update (it runs Node itself, with a warning, when no `meko` has `_launch`). |
| `scripts/package-release.sh`, `scripts/smoke-release.sh` | The release tarball for one platform (`present-<v>-<os>-<arch>.tar.gz`), and its check: version, start, the page over the socket, stop. |
| `test/` | Unit tests; integration tests with `FakeMeko` (in memory, Meko's artifact rules) and a stub `claude` (`test/fixtures/fake-claude.mjs`). |

## Invariants

1. **Only Meko reaches it.** The server listens on its unix socket alone (folder 0700). No login,
   port or TLS of its own; writes and WebSocket upgrades refuse another site's Origin; pages refuse
   framing. Every URL the pages build goes under the `<base href>`.
2. **Every edit goes through `applyOps` and the deck's rules.** Hand edits and Claude's alike; a
   change that breaks the deck is refused, not saved.
3. **Claude never lands over hand edits unseen.** Asks always come back as change sets; a map or
   build that would touch a hand-edited slide does too.
4. **A save never leaves a half-saved deck.** Pieces go up first, `deck.json` last; every piece is
   at most `PART_BYTES` (4 MiB), under Meko's 5 MiB artifact limit.
5. **Meko's key never reaches the browser or Claude's command line.** Claude gets it through
   `PRESENT_MEKO_KEY`, which its MCP config names.
6. **Limits are checked when an asset is added**: 100 MiB per asset, 1 GiB per deck. An asset's
   bytes are on this computer or in the deck's datapack before the deck names it.
7. **Treat the repo as public**: no internal hostnames, paths, tokens or keys, and no personal
   data: no real person's name, email, handle or other details, and no real deck or datapack
   content in tests or docs. The repo root's AGENTS.md has the full rule.

## Conventions

- TypeScript strict, ES modules, Node 22, AgentK's style. `src/shared` has no Node or DOM imports.
- User-facing text: plain, direct, active voice; errors say what went wrong and how to fix it.
- Tests: new behavior gets one test, in an `it.each` table when it's cases of one rule. Tests never
  touch the real Meko, Claude Code or home (`test/setup.ts`).
- Commits and PR titles name the `present` component (`feat(present/web): …`). A `feat`, `fix`,
  `perf` or `deps` subject naming it cuts a Present release (`.github/workflows/present-release.yml`,
  tags `present-v<version>`), which Meko installs and updates.
- Keep this file, `README.md` and `docs/design.md` current in the same commit as the code, and
  check changes against [REVIEW.md](REVIEW.md).
