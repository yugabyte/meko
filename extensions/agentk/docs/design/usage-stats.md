# Usage stats

Part of the [AgentK design](../design.md).

Each install sends a small report of counts once a day (every hour in its first day) to a
collector run by the AgentK team, so we know how many installs are active and how much work
goes through them. Release installs also send it to YugabyteDB's call home service
(diagnostics.yugabyte.com, described below), which is to replace the
AgentK team's collector. That collector is a
separate service (`services/ak-local-call-home` in yugabyte/agent-k). What it receives is
`CallHomeReport` in `src/shared/types.ts`, and [`docs/call-home-report.schema.json`](../call-home-report.schema.json)
is the same contract as a JSON Schema. The collector keeps an identical copy
(`internal/report/contract/call-home-report.schema.json`), and its tests tie that copy to its
types, enums, patterns and caps; change both together. A unit test here checks the reports the
server builds against the schema. `schema` in the report goes up when a
field changes meaning or is removed; adding a field doesn't change it. The collector stores an
enum value, model id, extension or version it doesn't know as `other`, and drops (only counting)
fields it doesn't know, rather than refusing the report, so an install that is ahead of it still
reports.

**What's in it** (`src/server/callhome.ts`, `buildReport`):

- *The install:* a random `installId` (made on first use, kept in `kv`; one per AgentK home, so
  hosts sharing a home count once), the version and update channel, platform, CPU, Node and
  Claude Code versions (`claude --version`; `''` when it can't be read; Kiro CLI's too, as `kiro`, while Kiro is on; our version or
  Node's, when the collector wouldn't keep it, such as a `git describe` build or a Node nightly, is
  cut to its leading numbers, else `0.0.0`), local or remote access, the service manager, whether Meko is on (and where its key comes from, and the
  sharing: `api` when AgentK can share through Meko's API, `none` when Meko is off or its API URL
  isn't known), and the UTC day of the first
  report.
- *All-time totals:* projects (current, deleted, with a dedicated datapack, shared datapacks
  attached, distinct repos), sessions (by kind, by the agent they run on (`byHarness`), open ones by stage; a task counts as `explore`,
  `plan` or `quick` by the stage it started in, Explore, Design or Code), what shipped (merged PRs,
  direct pushes and their commits as on the Usage page, plus the commits and lines of merged PRs),
  file types, tokens, idle worktrees, Meko use (below), and the tokens Meko brought into first
  turns (the Usage page's numbers).
- *Each UTC day* of the last 7 (30 in an install's first report, as far back as session events are
  kept): active projects and sessions, responses, projects opened (new, or opened again) and closed
  (from `project_events`, so a close a reopen undid still counts), sessions created
  by kind and archived, stage entries, PRs opened and merged with their commits and lines, direct
  pushes and commits, reviews handed to Claude, CI failures, review comment batches, recoveries,
  resumes, reused worktrees, file types, tokens by model, Meko use (left out of a day with none),
  and [Ask AgentK](palette.md#ask-agentk) asks (`agentk_asks`: asks, the ones that failed and the actions their
  answers proposed; left out of a day with none; a cancelled ask isn't counted), and Kiro (`kiro`:
  sessions started on it, its finished turns and the credits they billed, rounded to thousandths;
  left out of a day with none).
- *File types* are counts per file extension: files changed, lines added and deleted, from merged
  PRs and direct pushes. When a PR is merged, one `gh pr view <url> --json
  commits,additions,deletions,files` call adds its size to the `merged` event's payload; for a
  direct push it's `git diff --numstat -z`. Paths are reduced to extensions right away
  (`src/server/filetypes.ts`): the last dot-suffix, lowercased; `none` for no suffix or a
  dotfile; `other` for a suffix that isn't 1 to 10 letters and digits. Each list keeps the 25
  largest and sums the rest into `other`. GitHub lists up to 100 files per PR.
- *Meko use* is the Usage page's Meko numbers (`src/server/mekousage.ts`): calls, lookups that
  found something, came back empty or failed, saves, the tokens found lookups brought in, and the
  estimated savings (searches and seeds credited, tokens, and dollars in whole cents). It adds calls
  by tool, naming only the lookup and save tools (every other Meko tool counts as `other`), and
  AgentK's searches at session start by outcome with the milliseconds they took in all. Calls go on
  the day they were made, seeds and searches at start on the day the search ran. Added since
  (optional, so older installs' reports still check): about how many tokens Claude's calls sent to
  Meko (their input, characters ÷ 4), also by tool; [learn steps](recall-and-learn.md#the-learn-step) by trigger
  (`discard` optional, added later), path and outcome, the facts they proposed, added, corrected and skipped, the questions
  on What Meko doesn't know they marked taught and dismissed (`taught`, `dismissed`, optional, added later; how many they
  were offered isn't sent, since the 7-day report with every count at its largest has no room left in 64 KB), their Meko searches,
  failed calls and tokens sent, and their model calls' tokens (the fork, the duplicate check, and
  typed turns) with their price in whole cents; AgentK's own Meko calls (`meko_agentk_calls`,
  recorded in `MekoClient.call`: searches at start, designs, the learn step's) with
  failures, tokens sent, time taken and the 9 most used tools (the rest as `other`); and the Meko
  plugin's transcript captures AgentK ran (`meko_capture_runs`: runs, failures, time). What a
  capture sends isn't counted: AgentK can't see it. Each goes on the day it began, and is left out
  when there was none. Also optional: searches that found nothing relevant (`weak`), and the
  questions on [What Meko doesn't know](recall-and-learn.md#what-meko-doesnt-know) people dismissed (and
  not restored) and taught, each on the day of its latest dismissal or teach (asking again undoes
  neither, nor does a learn step marking it later: it counts what users did; a learn step's marks are in its counts
  instead), left out when there were none.

Never sent: project, session, repo, branch, datapack or file names, paths, prompts (and what's asked of Ask AgentK), designs,
transcripts or code, PR titles, URLs or numbers, user names, emails, GitHub logins, host names and
the server name, Meko keys or ids, and Meko queries, results and tool inputs. Model ids that aren't plain ids (a Bedrock ARN carries an
account number) are sent as `other`.

**Sending.** An hourly check (the first one 10 to 60 minutes after start, at random, so a release
doesn't make every install report at once) sends a report when the last one went out 23 hours ago
or more (`kv` `call_home_last_sent`), so restarts and updates don't send again. A new install
reports sooner and more often, so the collector sees it on the day it starts: until a day after
its first report (`kv` `call_home_first_sent`, the time of the check that sent it), the first
check comes 1 to 5 minutes after start (or when the next report is due, if that's later, so a
restart doesn't skip one), and a report goes out when the last one is 55 minutes old, so at every
hourly check. Reports are stamped with the check's time, so the time a send takes doesn't push
the next one past its check. Installs that reported before `call_home_first_sent` was
kept have none and stay daily. A report over the collector's 64 KB (a busy install's 30-day first report, with every day's file types and models
full) drops its oldest days until it fits, since the collector would refuse it every hour from then
on. It POSTs the JSON
to the collector's report URL, as given, with a 15-second timeout; a failure is logged once and tried again an hour
later, and nothing else waits on it. The collector upserts each day on `(installId, day)`, so a
resend changes nothing and a missed day is filled in by the next report. The URL is
`[call_home] url`, or else the one release builds carry: a `CALL_HOME_URL` file next to `VERSION`,
written by `scripts/package-release.sh` from the `CALL_HOME_URL` repo variable, so it never
appears in the repo. Dev servers and tests have neither, so they send nothing. Node's `fetch`
ignores `HTTPS_PROXY`, so an install behind a proxy doesn't report.

**YugabyteDB's call home service.** The same report also goes to `https://diagnostics.yugabyte.com`,
where yugabyted and YugabyteDB's other products report (a Flask app on App Engine,
`diagnostics_v2` in yugabyte/yugabyte-growth, writing to BigQuery `callhome_data`), so AgentK's
numbers sit with theirs. This is the call home that stays when AgentK's own collector is retired.
A second `CallHomeService` (target `yugabyte`) sends it on its own schedule, the same as above but
kept apart (`kv` `yb_call_home_last_sent`, `yb_call_home_first_sent`; the install id and first day
are shared), so one service being down never holds back the other. The body is yugabyted's
envelope with the report as its payload (`YbCallHome` in `src/shared/types.ts`):
`{callhome_type: "agentk", command: "heartbeat", timestamp, install_id, version, payload}`. The
service routes on `callhome_type` to its `agentk` table. `timestamp` is UTC with microseconds and
no zone (`2026-11-09T17:05:32.123000`), the only form it parses. It answers 200 even when it
couldn't store a row, so check a change to the envelope by finding the row in BigQuery. Only release
installs (with `AK_APP_DIR`) send there, unless `[call_home] diagnostics_url` names another URL.

**Turning it off.** It's on by default. The **Send anonymous usage stats to YugabyteDB** checkbox
in Settings → Account (under the password forms, or the local-access note) writes
`[call_home] enabled = false`; from then on no report is built or sent to either service, without a restart.
`YB_DISABLE_CALLHOME` set to a true word (`true`, `t`, `yes`, `y`, `1`) in the server's own environment,
yugabyted's switch, turns off both too, and the checkbox then shows off and disabled, naming it
(`Settings.callHomeOffBy`). Under the systemd or launchd service the server's environment is the
unit's or the plist's, not the shell's that ran `agentk start`, so a shell `export` doesn't reach it:
use the checkbox or `[call_home] enabled` there.

**Downloads** aren't sent from here: the collector reads every release asset's download count
from the GitHub API each hour, so a pruned release loses at most an hour of downloads.

**The collector's side of the contract.** `POST /v1/report` takes the report with no auth and
answers 2xx (anything else counts as a failure); it should cap the body at 64 KB, limit reports per
`installId` per day, check the schema, and store no IP addresses.
