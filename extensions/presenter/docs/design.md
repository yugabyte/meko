# Presenter: design

Presenter turns Meko datapacks into presentations that change during the talk. This is the design
as built; the decisions behind it, with the alternatives weighed, are in the "Decisions" table.

## Shape

```mermaid
flowchart TB
  subgraph Browser
    MUI["Meko's web UI at /"]
    PG["Presenter's pages at /ext/presenter/\ndecks, map, slides, console, audience"]
  end
  subgraph Meko["meko serve"]
    WL["web listener :7777\nlogin, proxy to the extension's socket"]
    AL["API listener 127.0.0.1:7778\nMCP and REST"]
  end
  subgraph P["meko_presenter serve (run/web.sock)"]
    API["API and WebSockets"]
    J["jobs: claude -p on the deck's session"]
    D["decks: drafts, ops, pieces, cache"]
  end
  MUI --> WL
  PG <--> WL
  WL --> API
  API --> J
  API --> D
  J --> AL
  D --> AL
  AL -.->|"when Meko links it, with the link's key"| MC["Meko Cloud\nMCP and REST"]
```

## Which Meko

Presenter keeps no Meko URL or key. It asks `meko token --json` (`src/server/meko.ts`), which
answers whether or not Meko's server runs:

| Meko says | Presenter uses |
|---|---|
| `cloud.linked` and `cloud.datapack_routing` | Meko Cloud and the Meko on this computer's own datapacks, through that Meko: its `url` and `token` for MCP and REST at the same origin's `/api/v1`, which serve a datapack that Meko has (one made there before the link) and pass any other id on to Meko Cloud. Presenter never sees the link's key. Meko Cloud is named by `cloud.web_url`'s host. With Meko's server stopped, no Meko |
| `cloud.linked` alone (a Meko from before it routed by id) | Meko Cloud, through the Meko on this computer: its `url` and `token` for MCP, which Meko passes on to Meko Cloud, and REST at the same origin's `/api/v1/cloud`, which Meko passes on to Meko Cloud's REST API |
| `cloud` unlinked or never set, or no `cloud` (a Meko from before the link) | The Meko on this computer: its `url` and `token`, REST at the same origin's `/api/v1` |
| `cloud` with `url` and `api_key` (an older Meko, which handed out the link's key) | Meko Cloud itself: MCP at `cloud.url`, REST at `cloud.api_url` (another host than the MCP one), the link's key. With no `api_url`, no Meko: the error says to link again |
| `cloud.error`, or `meko` failed or is gone, after a link was read | The link last read, kept in memory only, so a slow `meko` or a locked Keychain never moves decks |
| The same before any link was read (a start, or right after Meko's notice) | No Meko, rather than the one on this computer, for the same reason |

When the link changes, and at each start of Meko's server (an update or a rollback may move its REST
base, `datapack_routing`), Meko posts `{"what":"cloud"}` to `/_meko/changed` on Presenter's socket;
the client drops its connection, its conversation and the link it last read, and the next call finds
Meko again. A 404 from the Meko on this computer's REST API does the same, at most every 30 s, for a
rollback to a Meko that doesn't post it. One connection is made at a time, so the client and the credentials it reports are
always one Meko's. Each Claude run asks `meko token --json` again and reconnects when the URL or key
changed (the local Meko on a new port, a new client token), then gets that Meko's URL and key (the
key through `PRESENTER_MEKO_KEY`). A deck's id is its datapack's, which exists in one Meko only, so
each draft records the Meko it belongs to, read before Meko is asked to make or load it. A draft from
before Presenter kept it is the current Meko's when that Meko lists its datapack, else the Meko on
this computer's. A Meko Cloud draft named by another host (its MCP host, from when Presenter held the
link's key) becomes the current Meko Cloud's when that one lists its datapack. While the Meko on this
computer links Meko Cloud and routes by id, it lists its own datapacks with Meko Cloud's, so "the Meko
that lists it" no longer tells them apart: `localIds` reads its `GET /api/v1/datapacks`, where a row
whose `web_url` is a path (`/datapacks/<id>`) is its own, kept 30 s since every edit asks. A deck in
one of those is the Meko on this computer's: tagged so when made from an untagged draft or loaded,
served beside Meko Cloud's decks, and retagged when an earlier Presenter tagged it as Meko Cloud's
(which it did under a Meko that listed both sides before it routed by id), so it still opens once
Meko Cloud is unlinked; unlinked, the Meko on this computer listing it retags it too. While its list
can't be read, its own drafts aren't greyed out, and editing one says why it can't be checked. New decks are made in Meko Cloud and tagged as its own. The decks list shows another Meko's drafts greyed out, with what brings them back
(unlink Meko Cloud, or link that Meko Cloud again). Editing, saving and Claude's jobs refuse them,
checked again as each job and each Claude run starts, so a job queued before the link changed
doesn't run on the other Meko. A run of one plays on, so a talk under way outlives a change of the
link. The decks page says where decks are saved. An older
Presenter's `config.json` key is removed at install.

## The deck

A deck (`src/shared/deck.ts`) is a graph of **scenes**, each a linear run of slides, joined by
**edges**: `next`, `choice` (from a fork scene, with a label and a key 1 to 9) and `return` (back
to a hub). A **detour** scene has no incoming edge; the presenter jumps to it and comes back. Every
scene but a detour is reachable from the start. A **slide** has a layout, a variant per deck
variant (the first, `overview`, is required; `deep` goes further), notes, sources, and blocks:
heading, text (Markdown), bullets (optionally one at a time), metric, quote, table, image, video,
diagram (Mermaid), animation (a reference into an animation datapack) and code. Images and videos
name **assets**: files kept by SHA-256, at most 100 MiB each and 1 GiB a deck.

Every change is a list of typed operations (`src/shared/ops.ts`). `applyOps` applies them to a copy
and `deckProblems` checks the result; a change that breaks the deck is refused. Ids are looked up
among the deck's own keys only (`own`), never names every object inherits (`constructor`), and
the fields the pages show as text (notes, a quote's `by`, captions, sources) must be text. The editor's hand
edits, Claude's answers and change sets all go through it, and share one undo history.

## Building with Claude

Each deck has one Claude Code session, kept in the deck's folder (`decks/<id>/work`). Jobs on a
deck run one at a time: the first starts the session (`--session-id`, recorded as started as soon
as Claude names it, so a run cut short is resumed), later ones resume it (`--resume`). A build runs
up to three scenes at once, each on a fork of the session (`--fork-session`), so each starts from
what the map step read; with no deck session yet, or one Claude Code no longer has, a scene runs on
a session of its own. A scene's retry resumes its own session, and never replaces the deck's. Claude runs headless with the
user's own setup (all its tools, the user's MCP servers) plus Meko under the name `presenter_meko`,
`--permission-mode bypassPermissions`, hooks off, and answers in a JSON schema: a summary, a list of
operations, and files it made under `./assets`. Presenter checks the operations against the deck as
it is now; a bad answer is sent back once with what was wrong. Claude doesn't add assets itself
(`addAsset`): Presenter adds the files it lists, since only those have their bytes here.

- **Map** drafts or redraws the story map. **Build** fills scenes' slides. **Ask** changes a block,
  a slide, a scene or the deck from an instruction.
- A map or build that touches no hand-edited slide lands at once (Undo takes it back). An ask, or
  anything that would rewrite a hand-edited slide (a new file under the name of an asset one shows
  included), comes back as a **change set**: each operation with a tick, hand-edited ones unticked,
  applied in full or in part.
- Cancel stops a job whether or not Claude has started; stopping Presenter cancels its jobs.
- Every slide names its sources; the editor and console flag a slide without any.
- Timeouts: map and build 10 minutes a scene; an ask about a block or slide 3 minutes, larger 10.

## Storing a deck

A deck lives in its own datapack, `presenter/deck/<slug>`, as pieces (`src/shared/manifest.ts`,
`src/server/pieces.ts`), because Meko keeps an artifact to 5 MiB and can't delete one:

| Artifact | Holds |
|---|---|
| `deck.json` | The manifest: everything the map needs, and every other piece's hash and size. The newest is the current version. Over 1 MiB, it goes up in parts and `deck.json` points at them. |
| `scenes/<id>.json` | A scene's slides. |
| `assets/<name>` | A file of 4 MiB or less. |
| `parts/<sha256>.<n>` | 4 MiB parts of anything bigger, listed in order in the manifest. |

A save uploads only pieces whose hash the datapack doesn't have (Presenter keeps the hashes the last
loaded or saved version stands on, and every piece uploaded since, even by a save that failed), four
at a time, and `deck.json` last, so a save that stops partway leaves the previous version the
newest. Each manifest carries its save time, so its bytes always differ and the newest artifact is
the latest save. Loading reads the newest `deck.json` from the artifact list route, then its scenes;
assets come when a slide shows them, served from the cache in byte ranges (Safari plays video only
that way) with their hash as the ETag. A save that fails partway stops starting uploads and waits
for the ones under way before it reports, so the deck is never left marked as saving. Every piece fetched is checked against its hash, fetched again
once, then reported. A deck's working draft is kept in its folder after every change.

## Presenting

A run is a room on the server (`src/server/runs.ts`) that the console and the audience window both
join on `/ws/runs/<id>`. The server holds the position (`src/shared/run.ts`): scene, slide, build
step, the trail back from jumps and detours, the scenes shown, the variant, and whether the screen
is blanked. Runs are kept on disk, so a restart resumes them, and a deck edited mid-talk (by hand or by
Claude: the runs hear every change the deck store makes) moves the position, the way back from
jumps and Back's steps to the nearest places left. Back out of a jump forgets it. A window fits
the run to the deck it has, so it never shows a position in a scene it doesn't have, and a page
that throws says so instead of going blank. The audience window shows only the slide (with a fork's
options, and a small mark on a detour); keys work in both windows.

## Releases

Presenter releases on its own train (`.github/workflows/presenter-release.yml`, tags
`presenter-v<version>`, `scripts/release/*.sh --product presenter`). It has no native modules, so
one job builds it once and packages `presenter-<version>-<os>-<arch>.tar.gz` for linux-x64,
linux-arm64 and darwin-arm64 (`scripts/package-release.sh`), each with its own Node; a release
carries those and `SHA256SUMS`, and no installer.

Meko installs and updates them (`meko extension install presenter`, `meko extension update
presenter`, the Extensions page): it unpacks a release into `<home>/app/<version>`, checks that
`bin/meko_presenter version --json` names it, and points `app/current` and
`~/.local/bin/meko_presenter` at it. The command is `meko_presenter`, and the extension `presenter`
(its folder, tags, tarballs and pages). `bin/meko_presenter` is the launcher
(`packaging/meko_presenter`): it runs the bundled Node on
`lib/dist/server/cli.js`, with `PRESENTER_APP_DIR` naming the release's folder and
`PRESENTER_LAUNCHER` itself, and `meko_presenter start` runs `serve` through it. Run by `meko start`
and `meko stop`, which set `MEKO_SUMMARY=1`, `start` and `stop` print one row of Meko's summary
(`  ✓ Presenter       started`, `src/server/summary.ts`; the format is in Meko's
`backend/docs/install.md`) instead of their usual line.

An update is on probation until it has served for 5 minutes, and Meko runs it. The launcher hands
`serve` to Meko's launcher (`meko _launch presenter <launcher> serve`), which runs it again with
`MEKO_LAUNCHED` set, and then the launcher runs Node. Meko writes `app/update-pending.json` as it
switches `app/current`, counts each start of the new version, and on the start after three failed
ones points `app/current` back at the old version, adds the new one to `app/bad-versions` (which it
skips from then on) and runs the old one. A clean stop (`meko_presenter stop`: the server exits 0)
doesn't count, so only crashes do, and once the version has served 5 minutes Meko clears the
record and prunes old versions. These are the rules Meko follows for its own releases, written
once in Meko. With no `meko` that has `_launch` (one too old, or none on PATH, in `~/.local/bin` or
in Meko's `app/current/bin`), the launcher runs Node itself and says a failed update won't roll
back. A build from source has no launcher and no record, and Meko never updates it.

Meko updates Presenter on its own (its **Update automatically** switch, on unless turned off),
but not during a talk. Before an automatic update it runs `meko_presenter _busy`, which asks the
server on its socket (`GET /_meko/busy`) and exits 0 while a talk is live: a run has a window open
(the console or the audience's), or moved in the last 5 minutes (`Runs.live`; its windows may be
reconnecting). Meko then tries again at its next check. A server that isn't running, or doesn't
answer, isn't busy. An update someone asks for doesn't wait.

## Decisions

| Decision | Why | Weighed |
|---|---|---|
| A Meko extension with its own pages at `/ext/presenter/`, on a unix socket, as AgentK | One port and one login for everything Meko runs; one package, one release train | Pages in Meko's web UI (two apps on two trains); its own port (a second login) |
| A graph of scenes, linear inside | One model for linear, tree and graph decks, readable on a map | A graph of single slides; linked decks |
| Structured blocks, never model-written HTML | On-brand, editable by hand, safe to render | Free HTML per slide |
| Content from any datapack; names only for Presenter's own | Teams keep content where they already do | `presenter/story/...` datapacks |
| Two stages (map, then slides) with any tool Claude wants | Fix the shape before paying for slides; Claude reads, browses and draws | Meko's read tools only; one pass |
| One persisted session per deck, driven headless | Claude keeps what it read and the editing conversation; answers stay checked JSON | A fresh `claude -p` per call; a live terminal session |
| Typed operations for every edit, change sets for asks | One undo history, one set of rules, nothing unreviewed over hand edits | Rewriting the deck each time |
| Pieces under Meko's artifact limit | Decks of any size; only changes go up; never half-saved | One `deck.json`; knowledge files (indexed for search) |
| Meko installs and updates the releases; no installer or updater of Presenter's | Meko already stages, checks, switches and rolls back its own releases (`internal/update`); one place does it | An `install.sh` per release and a `presenter update` |
| The launcher hands `serve` to Meko's `meko _launch`, which counts starts, rolls back and finishes the update | A release that can't start goes back to the one before by itself, and the rules live in Meko once, for itself and every extension | Each extension's launcher counting and rolling back on its own; Meko watching the start from outside |
| `_busy` makes Meko's automatic update wait during a talk | A restart would drop the console and the audience's window mid-talk | Updating whenever a check finds one; turning automatic updates off before each talk |
| Meko's Meko Cloud link, read from `meko token --json` and followed on Meko's notice; Meko Cloud reached through the Meko on this computer | One place links Meko Cloud for every extension, and its key stays in Meko | A key in Presenter's own `config.json`; Meko handing extensions the link's key |
| Drafts name their Meko; another Meko's can be played, not edited | A deck's datapack exists in one Meko only; a talk under way outlives a change of the link | Hiding such drafts; letting their save fail with Meko's "not found" |
| One build job packages every platform | No native modules: only the bundled Node differs | AgentK's per-platform builds (needed for node-pty) |

## Not built yet

- Templates and animations from their datapacks, and the animation player (slides name an
  animation; the built-in template draws every deck).
- **Prepare** for an offline talk (fetching every piece ahead), live and prepared detours made
  during a talk, and run logs saved to the deck's datapack.
- A phone layout for the console, audience voting, and a PDF of the path taken.
