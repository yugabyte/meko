# Meko backend: usage stats

Meko sends usage stats to YugabyteDB's call home service, `https://diagnostics.yugabyte.com`, the
service yugabyted and YugabyteDB's other products report to. The stats show how many installs are active, on which versions and
platforms, and how much work goes through them, by client. AgentK sends its own report there too
([its usage stats](../../extensions/agentk/docs/design/usage-stats.md)).

## What's sent

`internal/callhome` POSTs yugabyted's envelope, with Meko's report as the payload:

```json
{
  "callhome_type": "meko",
  "command": "heartbeat",
  "timestamp": "2026-11-09T17:05:32.123456",
  "install_id": "<the install's random id>",
  "version": "0.20261101.1",
  "payload": {
    "schema": 2,
    "install": {"version", "commit", "platform", "arch", "go", "service", "auth", "secrets",
                "update": {"channel", "auto"}, "memoryEngine", "extensions": {"agentk": true}, "firstDay"},
    "totals": {"datapacks", "memories", "learnings", "promotedMemories", "artifacts", "conversations",
               "kbDocuments", "kbBytes", "dbBytes"},
    "days": [{"day", "calls", "errors", "ms", "byTool": {}, "byClient": {}, "tokensSent", "tokensSaved"}]
  }
}
```

- The install id is the store's `install_id` setting, made at first start.
- Schema 2: a day's `tokensSaved` is measured against the datapack's whole knowledge base (Meko
  Cloud's context efficiency); in schema 1 it was against the knowledge-base results a search returned.
- `days` covers the last 7 UTC days, or 30 in an install's first report, oldest first, every day
  present. A day counts the call log's MCP calls on that day (the log keeps 90 days).
- `byTool` names Meko's own MCP tools (`mcp.ToolNames()`) and counts anything else as `other`.
- `byClient` reduces each call's `agent_id` to its client, the part before `:`, matched against a
  fixed list (`callhome.Clients`: `claude_code`, `claude_desktop`, `cursor`, `codex`, `kiro`,
  `agentk`, `meko_cli`, `meko_web_ui`). AgentK's `agent-k-local` counts as `agentk`, no `agent_id`
  (`meko_agent`) as `none`, and anything else as `other`. So `claude_code:my-repo` is
  `claude_code`, and no repo name is sent.
- `memoryEngine` is the memory engine's own state when it answers (`loading` while it loads its
  models, as it usually is at a new install's first report, `ready` or `error`), else the server's view
  of it (`starting`, `restarting`, `failed`, or `unavailable` when it can't start). `extensions` lists every extension `meko install_extension` knows, installed or not.

Never sent: datapack, memory, conversation, artifact or file names and contents, queries, paths,
host names, user names and tokens. The service records the address the report came from, as it
does for yugabyted.

`timestamp` is UTC with microseconds and no zone (`callhome.Timestamp`). That's the only form the
service parses, and it answers 200 even when it can't store a report, so a 200 doesn't show that a
change to the envelope works.

## When

On AgentK's schedule. The first check comes 1 to 5 minutes after start for a new install (no
report yet, or its first under a day ago), else 10 to 60 minutes after, at random, so a release
doesn't make every install report at once. Then there's a check every hour. In an install's first
day a report goes out at every check (55 minutes apart); after that, 23 hours after the last one.
The settings `call_home_last_sent`, `call_home_first_sent` and `call_home_first_day` keep the
schedule across restarts. A report is stamped with its check's time. A failure (no answer within 15
seconds, or anything but a 2xx) is logged once and tried again at the next check; nothing else
waits on it. The sender runs with the server's other background jobs and stops with them.

## Where

`[call_home] url` (or `MEKO_CALL_HOME_URL`), else `https://diagnostics.yugabyte.com` from a
release install only. A build from source with no URL sends nothing, so tests and dev servers never
report. The release smoke test (`scripts/smoke-release.sh`) sets `MEKO_CALL_HOME=false`.

## Turning it off

Usage stats are on by default. Each of these turns them off:

- The **Send usage stats to YugabyteDB** checkbox in the web UI's Settings (the gear in the
  sidebar's footer). It's the `call_home_enabled` setting in the store, through `GET` and
  `PATCH /api/v1/settings/usage-stats`, and the next check reads it, with no restart.
- `[call_home] enabled = false` in `config.toml`, or `MEKO_CALL_HOME=false`.
- `YB_DISABLE_CALLHOME` set to one of yugabyted's true words (`true`, `t`, `yes`, `y`, `1`).

The two variables count only in the environment of the process that runs `serve`. Under the login
service that's the systemd unit's or the launchd plist's (`MEKO_SERVICE` alone), not the shell's that
ran `meko start`, so an `export` there doesn't reach it. Use the web UI or `config.toml` for a
release install; the variables suit a server run by hand, or CI.

Config and the environment can only turn usage stats off. When they do, the web UI's checkbox is off
and disabled, and says what turned it off (`config.toml`, `MEKO_CALL_HOME` or
`YB_DISABLE_CALLHOME`). `meko status --verbose` says whether they're on, where they go (YugabyteDB, or
the URL config gives) and what turned them off. `MEKO_CALL_HOME=true` lifts `config.toml`'s
`false`, but not `YB_DISABLE_CALLHOME`. Meko never writes `config.toml`, so the web UI's setting
lives in the store. Meko's switches don't turn off
AgentK's usage stats, and AgentK's don't turn off Meko's.

## Tests

- `internal/callhome`: the envelope and report from a real store, checked to hold no seeded name;
  the client and tool reduction; the timestamp form; a report the service took is recorded even when
  serve stops right then; the schedule (hourly the first day, then daily,
  across restarts); the first delay; the switches; a failure retried and logged once.
- `internal/store`: `UsageTotals` and `CallDays`.
- `internal/config`: `[call_home]`, `MEKO_CALL_HOME`, `MEKO_CALL_HOME_URL` and `YB_DISABLE_CALLHOME`.
- `internal/rest`: the setting's routes, including a forced off.
- `internal/cli`: a started server sends a heartbeat to `MEKO_CALL_HOME_URL`, and `status` says
  what turned usage stats off.
