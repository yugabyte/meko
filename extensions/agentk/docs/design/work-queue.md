# The work queue

Part of the [AgentK design](../design.md).

One ordered list of what the user is working on, across every project (`/queue`: **Work queue**
in the rail under Usage, the list icon beside Settings in the rail's footer, and the command palette). Items are added by hand or pulled in by a sync
from GitHub and Slack. Each one has links (GitHub, Jira, Slack, any page) and at most one AgentK
session, whose status dot it shows. The server keeps it in `state.db` (`src/server/workqueue.ts`)
and pushes the open items in `AppState.workQueue`, so every device shows the same queue.

## Items and links

`work_items` holds the title, notes, `position` (lowest first), `status` (open or closed),
`source` (`manual`, `github`, `slack`), `source_key` (a synced item's link key), `session_id`,
`unplaced` and `github_done_at`. `work_item_links` holds each link in its order, with the key it's
matched by. `parseWorkLink()` (`src/shared/worklinks.ts`, shared by the server and the palette)
reads a link and gives every spelling of the same thing one key:

| Link | Key | Chip |
|---|---|---|
| `github.com/<owner>/<name>/pull/<n>` or `/issues/<n>`, or `owner/name#<n>` | `gh:<owner/name, lowercased>#<n>` | `<name>#<n>` |
| `…/browse/KEY-<n>` or `?selectedIssue=KEY-<n>` on any site, or the bare key | `jira:KEY-<n>` | `KEY-<n>` |
| `<workspace>.slack.com/archives/<channel>/p<ts>` (any query) | `slack:<channel>/p<ts>` | `Slack` (`Slack #<channel>` from a sync) |
| any other http(s) link | `url:<the link>` | the host |

A new item goes to the bottom. One added with no title takes its first GitHub link's title
(`issueOrPr()`). Closing an item keeps its links and session, so the palette still finds them;
reopening puts it at the bottom.

**Order.** Dragging a row (not on touch screens) sends `PUT /api/work-items/order` with every open
item's id in the new order and the one that moved. A list that isn't exactly the open items is a
409, so two devices reordering at once can't scramble it. Each row's ⋯ actions also have Move to top,
Move up and Move down (`PATCH … {move}`), which is how a phone reorders.

## One session per item

`work_items.session_id` is unique: an item has at most one session and a session is on at most one
item. `setItemSession()` is the only writer. It takes the session off any other item first, and
replaces the item's own. `Session.workItemId` is read back from it.

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
  session's ⋯ sheet and the palette (`LinkWorkItemDialog.tsx`), which says when a pick moves a link.
- **A closed session** keeps its link: the item says its session is closed and offers Start session
  again, which replaces the link.

The item shows its open session's `StatusDot`, name and stage from the state the rail renders, so it
updates live. The rail's Work queue entry shows its sessions' most urgent dot (`queueSummary()`) and
how many items are open and new.

## Sync

**Sync** (`POST /api/work-items/sync`) and a timer every `[work_queue] sync_minutes` (15 by default;
0 turns it off) add to the bottom of the queue:

- **GitHub:** the open PRs asking for your review (`gh search prs --review-requested=@me --state=open
  --limit 100`, `reviewRequests()` in `github.ts`), titled `Review <owner/name>#<n>: <title>`.
- **Slack:** the messages on your Later list, read by one `claude -p` through your own Slack
  connector (`src/server/slack.ts`), titled with their first line. Unlike Ask AgentK, it loads your
  MCP servers and claude.ai connectors. In `-p` nothing can be asked, so only the tools in
  `[work_queue] slack_tools` run (the claude.ai Slack connector's search and read tools by default),
  and its write tools and the built-in tools are denied outright, whatever your own settings allow.
  The answer follows a JSON schema; `found: false` (no Slack tools, or no way to the list) is an
  error, not an empty list. It needs Claude Code; the model is `[work_queue] slack_model`.

Each source succeeds or fails on its own, and `AppState.workSync` carries how each went for the line
beside Sync. A link already on any item, open or closed, is never added again, so closing a synced
item keeps it gone. A synced item is `unplaced` (the **New** chip) until it's moved or **Keep here**
is pressed. A GitHub item that a sync no longer finds (merged, closed, or your review no longer
requested) gets `github_done_at` (**Done on GitHub**); it's cleared if the review is requested again.
Nothing closes by itself.

Reading Slack is a model call, so the timer reads it only while the last Slack read worked: a
Sync you press that reaches Slack turns that on (`kv` `work-slack-auto`), and any failure turns it
off until a Sync you press works again. GitHub is read on every run.

## The command palette

A pasted GitHub, Jira or Slack link, `owner/name#<n>` or a bare Jira key (`workLinkKey()`) is looked
up once typing pauses (`GET /api/work-items/by-link`, an indexed lookup in `state.db`, no tokens).
`workLinkEntries()` in `src/web/palette.ts` lists the session of each item holding it first (Go to,
or a closed one's PR), then each item (opening the queue at it), and Start a session for an open item
with no open session. For a GitHub link these come before the link's own items, and a session both
name is listed once. The palette also has **Work queue**, **Add a work item**, **Sync the work
queue**, and Link, Unlink or show the work item of the session on screen.
