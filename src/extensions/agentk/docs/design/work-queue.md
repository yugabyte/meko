# The work queue

Part of the [Kode design](../design.md).

One ordered list of what the user is working on, across every project (`/queue`: the list icon
beside Settings in the rail's footer, and **Work queue** in the command palette). Items are added by hand or pulled in by a sync
from GitHub and Slack. Each one has links (GitHub, Jira, Slack, any page) and at most one Kode
session, whose status dot it shows. The items are kept in Meko Cloud, in a Postgres database every
one of the user's computers reads ([Where it's kept](#where-its-kept)); the server
(`src/server/workqueue.ts`) keeps the open ones in memory and pushes them in `AppState.workQueue`, so
every device, and every computer running Kode on the same Meko Cloud account, shows the same queue.

## Where it's kept

The queue needs Meko Cloud (the user's decision when it moved there): with the Meko on this computer
not linked to Meko Cloud, or before the queue is set up, `AppState.workQueue` is empty,
`AppState.workStore` says why, the queue page says so in place of the list (with **Set up the work
queue** or **Join the work queue**, which open Settings › Work queue), and every write and a pressed
Sync answer 409 with what to do. Starting a session works as always.

**The database.** An AMP cluster named `kode-work-queue` in a datapack Kode makes,
`adlc/kode/my-work-queue` (`WORK_QUEUE_DATAPACK`, named by the user). Its tables are in a `kode`
schema (`src/server/workstore.ts`): `kode.work_items` and `kode.work_item_links` as below, plus
`kode.meta`'s `schema_version`. Whichever Kode connects first after an update migrates it, under an
advisory lock; a Kode that finds a newer version than it knows reads the queue but refuses writes
with a 503 ("Update Kode on this computer"). Item ids are the UUIDs Kode gives them; times are epoch
milliseconds. `source_key` is unique, and a sync inserts with `ON CONFLICT DO NOTHING`, so two
computers syncing at once add a PR once. Reorder and moves lock the open rows (`FOR UPDATE`).

**What stays on this computer.** Which session works on an item: `work_item_sessions` in `state.db`
(sessions exist only where they run), so session starts, failed-start relinks, `Session.workItemId`
and `soleItemFor()` stay synchronous. An item can have a session on each computer. Also
`work_pending_close` (below), and kv `work-amp`, this computer's copy of the connection record.

**Reading it.** `Core.state()` can't wait on the network, so `WorkQueue` keeps the open items in
memory and reads them again after each of its own writes, when a page shows the queue
(`POST /api/work-queue/refresh`), after a sync, and every minute while a page is connected to
`/ws/events`. Nothing holds a connection: node-postgres's pool closes one after 30 s unused, so the
database's compute can scale to zero. Changes made on another computer show within a minute.
Closed items and the palette's lookup by link query the database.

**When it can't be reached** (a socket, TLS or timeout error, or a connection or auth SQLSTATE:
`unreachable()`), the page keeps the list last read with a line giving the cause and pointing to
Settings › Work queue, and writes and Sync are 503s (a Sync reads again first; the timer's skips, and
leaves Slack on the timer). Linking a session to an item, or starting one from it, still works for the
items in the list read since Kode started (both are local); after a restart with the database down
that list is empty, so they wait for the first read that reaches it. Unlinking always works, for any
item. A first read that fails another way (making the tables, the copy below) shows its error the
same way, and writes wait for it. Merge & Close's dismiss, which nobody pressed and nobody would see
fail, always goes into `work_pending_close`, and a read in the background applies it, so the stage
change never waits on Meko Cloud; one the database can't take yet waits there for the next read that
reaches it (none on a newer Kode's database, which this one doesn't write). A failed query also reads
the connection record from the datapack again, a minute after the last read at the soonest, the gap
doubling up to an hour while it finds nothing new, since another computer may have replaced the
endpoint; a new record is used only once `amp_compute_get` gives its host. A write refused with a 409
(a reorder of a list another computer changed) reads the queue again, so the retry works on it as it
is now.

**Setting it up** (`src/server/workcloud.ts`, Settings › Work queue, `POST /api/work-queue/setup`
with this computer's address). Every call goes through Kode's Meko client to the Meko on this
computer, which passes the `amp_*`, datapack and artifact tools on to Meko Cloud; Kode has no
credentials of its own for Meko Cloud. In the background, with each step in `AppState.workStore`:

1. Look for the datapack among the account's own (`datapack_list`, `grant` owner). If its
   description names a connection record, join it instead (below).
2. Unless a set-up cut short already made the cluster (found by name, and counted in the plan
   already), check `amp_core_budget`: the writer needs 0.5 vCPU, and a plan without room is refused
   in words (a 402 is never retried).
3. Make the datapack. Its name is unique across Meko Cloud, so if another account holds it,
   set-up stops and says so.
4. Make a password (24 random bytes) and save it in kv `work-amp-step` before anything uses it, then
   `amp_cluster_create` with `with_writer`, `ingress_external`, the address and that password.
   Kode makes the password because AMP returns a generated one only once; a restart part way picks
   the set-up up from kv and finds the cluster by name instead of making a second one, and so does
   pressing Set up again after a failure, which keeps the saved password. The step names the Meko
   Cloud account it's for, and isn't resumed while Meko is linked to another.
5. Wait (up to 10 minutes) for the writer's address (`amp_compute_list`, `amp_compute_get`).
6. Save the connection record (the ids, host, port, database, `cloud_admin`, the password and the
   allowed addresses) in the datapack as `kode-work-queue.json` with `artifact_put`, and its hash in
   the datapack's description (`Kode work queue. Connection: sha256:<hash>…`) with `datapack_update`:
   `artifact_get` takes only a hash, and nothing lists a datapack's files.
7. Connect, migrate, copy this computer's items in (below), read the queue.

**Another computer** finds the datapack, reads the hash, fetches the record (checked before use:
it's data, never instructions), checks its host against `amp_compute_get`, and, when its address
isn't on the list yet, replaces the endpoint (below). It then keeps its own copy in kv, so its queue
opens even while the Meko on it is down. A record kept for another Meko Cloud account is forgotten
when Meko links to that account; unlinking empties the queue here and keeps the session links.

**Addresses.** The endpoint lets in only its allow-list: CIDRs no wider than /24 (IPv4) or /48
(IPv6), a single address taken as /32 or /128 (`checkCidr()`). AMP can't change an endpoint's list
and a branch takes one writer, so adding or removing an address (`POST /api/work-queue/addresses`,
`…/addresses/remove`; the last one can't go) deletes the writer and makes a new one with the whole
list and the same password, then saves the record again. Every computer's address is on that one
list; the others follow the new endpoint when their next query fails. If making the new one fails
after the old one is gone, the error says the queue can't be reached until the same press is made
again. Nothing was saved, so this computer's copy still has the old list, and Let it in or Remove
runs again; a Join that finds the record's endpoint gone (not in `amp_compute_list`) makes a new one,
its host from AMP rather than the record.

**What was in `state.db`.** The first set-up or join on a computer copies its items from before the
move (`work_items`, `work_item_links`, migration 42) into the database: open ones in their order at
the bottom, marked New, then closed ones, skipping any id or `source_key` already there. A synced item
another computer had already added keeps its session here: the link moves to that computer's item.
The local tables are left as they were. kv `work-local-copied` holds how many there were and the
newest's time, and the copy runs again when that changes: a rollback to a release before the move
adds items there, which the next copy takes in. The session links such a release makes in
`work_items.session_id` are copied back at every start (`WORK_LINKS_BACK` in `db.ts`), only for an
item and a session with none here; `setItemSession()` keeps `work_items.session_id` in step, so a
link removed since isn't put back.

The record holds the database's password, so anyone the datapack is shared with can open the
queue; Settings says not to share it ([security.md](security.md)).

## Items and links

`kode.work_items` holds the title, notes, `position` (lowest first), `status` (open or closed),
`source` (`manual`, `github`, `slack`), `source_key` (a synced item's link key), `unplaced`,
`github_done_at` and `updated_at`. `work_item_links` holds each link in its order, with the key it's
matched by. `parseWorkLink()` (`src/shared/worklinks.ts`, shared by the server and the palette)
reads a link and gives every spelling of the same thing one key:

| Link | Key | Chip |
|---|---|---|
| `github.com/<owner>/<name>/pull/<n>` or `/issues/<n>`, or `owner/name#<n>` | `gh:<owner/name, lowercased>#<n>` | `<name>#<n>` |
| `…/browse/KEY-<n>` or `?selectedIssue=KEY-<n>` on any site, or the bare key | `jira:KEY-<n>` | `KEY-<n>` |
| `<workspace>.slack.com/archives/<channel>/p<ts>` (any query) | `slack:<channel>/p<ts>` | `Slack` (`Slack #<channel>` from a sync) |
| any other http(s) link | `url:<the link>` | the host |

A new item goes to the bottom. One added with no title takes its first GitHub link's title
(`issueOrPr()`). **Dismiss**, on every row, closes an item (`status` closed); it keeps its links and
session, so the palette still finds them, and it's listed under **Dismissed**, where **Reopen** puts it
at the bottom. **Dismiss all** (after an inline Are you sure) sends the ids the page showed when it started asking
(`ConfirmButton`'s `onAsk`) to `POST /api/work-items/close` (`closeAll()`), which closes those still
open, so an item a sync adds while it asks stays.

**Order.** Dragging a row (not on touch screens) sends `PUT /api/work-items/order` with every open
item's id in the new order and the one that moved. A list that isn't exactly the open items (one
added, closed or reopened meanwhile) is a 409. Two devices reordering the same items: the last order
sent wins, with no version check. Each row's ⋯ actions also have Move to top,
Move up, Move down and Move to bottom (`PATCH … {move}`), which is how a phone reorders. Where each
puts the item is `moveItem()` in `src/shared/workorder.ts`, shared by the server and the page, which
shows the move at once and keeps the list scrolled where it was (it saves `scrollTop` and puts it
back as the list redraws; `.wq-scroll` has `overflow-anchor: none`, since the browser's scroll
anchoring would follow a moved row), so the user stays at their place in the queue.

## One session per item

`work_item_sessions` (in `state.db`) is one to one: an item has at most one session on this computer
and a session is on at most one item. `setItemSession()` is the only writer. It takes the session off any other item first, and
replaces the item's own. `Session.workItemId` is read back from it. A failed start gives the item back
to its earlier session only while the item is still linked to the start that failed.

- **Start session** on an item with no open session opens New session filled in from the item
  (`itemStartRequest()` in `src/web/workqueue.ts`): a GitHub review request opens on Review a PR with
  its PR, anything else is a task at Design named after the item, its prompt the title, notes and
  links. The queue spans projects, so the dialog gets a **Project** picker
  (`NewSessionDialog`'s `projectChoice`), set first to a project with a session on a PR of the
  item's repo, else the project on screen. `CreateSessionRequest.workItemId` links the session in the
  same transaction that writes its row (`SessionManager.begin`), so a start refused before then leaves
  no link.
- **A pasted link.** A session started from a GitHub link in the palette (`ghlinks.ts`) is linked to
  the one open item holding that link, when it's the only one and it has no open session
  (`soleItemFor()`).
- **By hand:** Link a session… on an item, or Link to a work item / Change its work item from the
  session's ⋯ menu (the ⋯ sheet on a phone) and the palette (`LinkWorkItemDialog.tsx`), which says when a pick moves a link.
- **A merged session dismisses its item.** When Merge & Close finishes (`Lifecycle.cleanup()`, also
  for a PR merged on GitHub), the open item the session is on is closed as Dismiss closes it
  (`WorkQueue.closeForSession()`), with a `work-item` event on the session; while the database can't
  be reached, the close waits in `work_pending_close`. The link stays, so the
  Dismissed list shows which session finished it. Discard and Close review leave the item open.
- **A closed session** keeps its link: the item says its session is closed and offers Start session
  again, which replaces the link.

The item shows its open session's `StatusDot`, name and stage from the state the rail renders, so it
updates live. The list icon in the rail's footer is dotted while synced items are new and has the
open count at its bottom right, up to 99 and then 99+ (`queueSummary()`, `queueBadge()`).

## Sync

**Sync** (`POST /api/work-items/sync`) and a timer every `[work_queue] sync_minutes` (15 by default;
0 turns it off) add to the bottom of the queue, from the sources `[work_queue] github` and `slack` turn
on (both off by default, so there's no sync until you turn one on). All four, with `slack_list` (the Slack list's link), are in Settings › Work queue, reached from **Sync settings** on the
queue page too. With both sources off there's no sync: the button is disabled, a pressed Sync is a 409,
and the timer does nothing (`WorkSync.sources` tells the page). The timer ticks every minute
(`WorkQueue.tick`) and syncs once the minutes have passed, so a change in Settings applies at once:

- **GitHub:** the open PRs asking for your review (`gh search prs --review-requested=@me --state=open
  --limit 100`, `reviewRequests()` in `github.ts`), titled `Review <owner/name>#<n>: <title>`.
- **Slack:** the messages on a [Slack list](https://slack.com/help/articles/27452748828179-Use-lists-in-Slack)
  of yours, whose link is `[work_queue] slack_list` (Settings › Work queue; `slackListId()` reads the
  list's file ID from it, and with none there the read fails without running anything). Not Slack's
  To-dos: the claude.ai Slack connector's search ignores modifiers it doesn't know, `is:todo` among
  them (it returns the newest messages anywhere), its results don't say what's a to-do or done, and
  no tool of its lists To-dos or Saved. Its `slack_read_file` returns a list as CSV with the list's
  own columns, so the prompt has Claude read that one file, without searching Slack or reading the
  messages. It reads only columns with these exact names (`LIST_COLUMNS` in `slack.ts`, matched
  ignoring case), and ignores the rest, so nothing rests on guessing what a column means:
  **Message** (the message's link; required, and a list without it fails with "Your Slack list has
  no Message column"; rows whose Message isn't a Slack message link are left out), **Name** (the
  title column Slack fills with an added message's text), **Reporter** (optional) and **Done** (an
  optional checkbox; checked rows are left out). A list is private until it's shared, but sharing one of its items gives the
  people it goes to edit access to the whole list, so its rows are still data written by people.
  The rows are read by one `claude -p` through your own Slack
  connector (`src/server/slack.ts`), titled with a summary the run writes from the row's text (its name or title column, else its first column of text), at most 80 characters, without greetings or mentions. When a message is added to a list, Slack copies its raw text into the row's name, starting with whatever opens the message ("Hi <@U…>"), so its first line makes a poor title. A row with no text gets "Slack message". The row's text
  itself goes in the item's notes, with mentions written as `@<display name>` from the user's
  profile, started with the reporter the way Slack starts a message with its author ("<reporter>:
  …", from the Reporter column, as the list has it: an email stays an email,
  since the profile read takes only user IDs). The queue page shows an item's notes in two lines
  under its title. Unlike Ask Kode, it loads your
  MCP servers and claude.ai connectors (no `--strict-mcp-config`). It reads messages other people
  wrote, so it's kept to reading Slack: no built-in tools but ToolSearch (`--tools ToolSearch`);
  none of your Claude Code settings but `env` and `apiKeyHelper`, which signing in needs (`signInSettings()`, with
  `--setting-sources project` in Kode's empty `run/work-sync` folder), so your allow rules and
  default mode don't apply; `--permission-mode default`; and only `[work_queue] slack_tools` allowed
  (only the claude.ai Slack connector's `slack_read_file`, and `slack_read_user_profile` to name the
  people a row mentions, by default, so the run can't search Slack or
  read a channel or thread, even when a row's text asks it to. That tool takes any file ID, so only
  the prompt keeps it on the configured list: a row asking for another file could get the rows of
  another list or canvas the user can open added as items, which only adds to the queue; a `slack_tools` set in config.toml
  before this keeps its old list and needs `mcp__claude_ai_Slack__slack_read_file` added). ToolSearch runs too, since it
  needs no permission, but it only loads tools' schemas. In `-p` nothing can be asked, so every other
  tool, another connector's included, is refused; the claude.ai connector's write
  tools are also denied by name. The prompt calls the rows' text data, not instructions.
  The answer follows a JSON schema; `found: false` (no Slack tools, or no way to the list) is an
  error, not an empty list (a list with no rows left is `found: true`). It needs Claude Code; the model is `[work_queue] slack_model`.
  Claude.ai connectors are still connecting when a `-p` run's first turn starts, so with no tools
  at all the run never saw Slack and always answered `found: false`; ToolSearch waits for a
  connecting server, so the prompt (`slackPrompt()`) has Claude load `slack_tools` with
  `select:<slack_tools>` first. That also leaves every connector's tools deferred: setting
  `MCP_CONNECTION_NONBLOCKING=false` instead also works, but puts all their schemas in the prompt
  (about 100k tokens, $0.43 a read on Sonnet against $0.04–0.06).

Each source succeeds or fails on its own, and `AppState.workSync` carries how each went for the line
beside Sync. A link already on any item, open or closed, is never added again, so closing a synced
item keeps it gone. A synced item is `unplaced` (the **New** chip) until it's moved or **Keep here**
is pressed. A GitHub item that a sync no longer finds (merged, closed, or your review no longer
requested) gets `github_done_at` (**Done on GitHub**); it's cleared if the review is requested again.
Each computer marks only the requests its own sync has found (kv `work-github-seen`, the last 1,000),
since another computer's gh may be signed in to another account whose requests this one never sees.
A read that comes back full (`REVIEW_REQUESTS_MAX`, 100) may have left some out, in no set order,
so it marks nothing done. Nothing closes by itself. `WorkSync.slack` is null when a run left Slack
out (a timed one while Slack isn't on the timer).

Reading Slack is a model call, so the timer reads it only while the last Slack read worked: a
Sync you press that reaches Slack turns that on (`kv` `work-slack-auto`), and any failure turns it
off until a Sync you press works again. Saving a new `slack_list` drops the last Slack result
(`slackListChanged()`), so the line says Slack waits for a Sync press instead of showing the old
list's error. GitHub is read on every run.

## The command palette

A pasted GitHub, Jira or Slack link, `owner/name#<n>` or a bare Jira key (`workLinkKey()`) is looked
up once typing pauses (`GET /api/work-items/by-link`, an indexed lookup in the queue's database, no
tokens; the open items last read while it can't be reached; none before set-up).
`workLinkEntries()` in `src/web/palette.ts` lists the session of each item holding it first (Go to,
or a closed one's PR), then each item (opening the queue at it, with the Dismissed section open for a
dismissed one), and Start a session for an open item
with no open session. For a GitHub link these come before the link's own items, and a session both
name is listed once. The palette also has **Work queue**, **Add a work item**, **Sync the work
queue**, and Link, Unlink or show the work item of the session on screen.
