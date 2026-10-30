# Sharing a project

Part of the [AgentK design](../design.md).

A project is shared by sharing its dedicated datapack. Meko shares a datapack as an **invite**: the
owner (or a maintainer) invites an email address with a role, Meko emails them, and they accept or
decline it. Roles are `contributor` (runs sessions and writes their own memories; the default),
`maintainer` (can also promote memories and invite others) and `viewer` (read only). Memories are
per user in Meko, so a member sees their own memories plus the datapack's shared knowledge
(promoted memories and knowledge-base files), not the owner's raw memories.

Meko's share routes (`POST /datapacks/:id/share`, `GET …/member-list`, `GET …/outgoing-shares`,
`POST …/invite/:id/resend`, `DELETE …/invite/:id`, `GET /datapacks/incoming-shares`,
`POST /datapacks/invite-action`, and the share link's `GET`/`PUT …/share-link` and
`POST /share/:token/redeem`) are REST, not MCP. `src/server/mekoapi.ts` calls them with the
same API key (a Meko personal access token), at `[meko] api_url` (by default the web app URL plus
`/api/v1`). AgentK can share when Meko is on and that API URL is known (`resolveMekoApiUrl()`, from
the config, not by asking Meko); the state sent to the browser carries it as `MekoStatus.sharing`.
Without an API URL the share routes answer 409, saying to set the web app URL, and the invite sync
skips Meko's invites. The Meko on this computer has no share routes and usually no API URL, so
while AgentK uses it, Share doesn't open the Share dialog. (An API URL on its own origin, kept in
config.toml from before Meko owned the link, is the exception: `forKey()` in `config.ts` keeps it,
and Share then opens the Share dialog.) It opens **Share through Meko Cloud** instead
(`MekoLinkDialog.tsx`), as Meko's own Invite points to Meko Cloud: **Sign up for Meko Cloud ↗**
(`MEKO_CLOUD_SIGNUP_URL`, Meko Cloud's sign-up, landing on its Settings where the API key is made,
in a new tab) and **Link Meko Cloud** (Meko's `/settings/meko-cloud`). Any other Meko without an
API URL gets **Set Meko's web app URL**, which links to that same page of Meko's. Meko's own refusals of a share call (a bad email, an invite already pending,
no permission) come back with their status and message; a refused key (401) is a 502 that says to
check the key in Meko's web UI (Settings › Meko Cloud); anything else is a 502. The invite sync keeps asking every five
minutes, but logs a failure to list the invites waiting (a refused key, say) once, until the error
or API URL changes or a sync works.

| | |
|---|---|
| Share | The Share dialog: the reviewers of the project's open PRs (below), emails, role (Viewer, Contributor, Maintainer, least to most; Contributor by default), the people with access and pending invites (Resend, Revoke). The Emails field takes a list separated by commas, semicolons or whitespace (new lines and tabs pasted from a spreadsheet become commas); `Jane Doe <jane@example.com>` counts as the address in `<…>`, and repeats are sent once. Anything that isn't an address stops the invite and is named. Addresses the dialog's lists show as members or pending invites are left out and named (while the lists load, or when they can't be read, those go to Meko, which refuses a repeat invite). The server invites up to 50 one after another (`emails` in `ShareDatapackRequest`) and reads the lists once: when some fail it answers with who was invited and who wasn't, and why (`ShareDatapackResult`), and the failed ones stay in the field; when none go through it fails with the first error. A refused key or an unreachable Meko stops the list, so a Meko that times out costs one 30-second wait, not one per address. Meko has no route to remove someone who has accepted; that's done in Meko. When an invite goes through but the lists can't be read again, it still says the invite was sent (`refreshError`). |
| Share by link | The Share dialog's first section (`ShareLink.tsx`): the datapack's one Meko share link, which anyone signed in to Meko who opens it joins with, no invite needed. Off, it shows **They join as** (Viewer or Contributor, neither picked: you pick each time) and **Turn on link**, disabled until you pick. On, it shows the link in a read-only field with **Copy link** (selected in the field when the browser won't copy), the role, how many joined through it, a warning that changing the role also changes it for the people who joined at that role, **Turn off**, and **Reset link** (asks first; the old link stops working, people who joined keep access). Maintainer isn't offered: Meko doesn't allow it for links. Meko takes one change per call, so turning on is `{status: "active"}`, then `{grant}` when Meko kept another role; anyone who joins in between moves with the role change. When a change fails, the section reads the link again to show it as it is. The link is `{web app}/share/{token}` (`shareLinkUrl()` in `src/shared/mekourls.ts`), so it needs the web app URL: without one the section says to set it. A 403 (you aren't the owner or a maintainer) shows one line saying so; a Meko without the route (405, 501, or a 404 with no error code: `routeMissing()` in `mekoapi.ts`) hides the section. The token reaches only the browser of someone signed in to AgentK: it isn't logged or stored. |
| Join by link | A share link pasted into Ask AgentK (the command palette) gives one item, **Join the datapack this link shares**, only for a link on this Meko's web app (same origin and path: `shareLinkToken()`), so a look-alike link's token never reaches Meko. Pasting joins nothing; picking the item does (`POST /api/meko/share-links/join`): Meko's redeem, then one `datapack_list` the shares are synced with. `InviteService.joinedHere()` marks the datapack (and bumps the sync's generation, so a sync that read Meko before the join reads it again), so that sync records it as `later`, even on the first sync: listed in Open project, with no banner or notification, since the person who joined knows. It isn't taken for gone while Meko's list catches up. The result's `grant` is our access as the datapack list has it (a maintainer who pastes a viewer link stays a maintainer), else the link's role. A redeem route Meko doesn't have (`routeMissing()`) says this Meko has no share links. A contributor share opens the Open project dialog with it chosen; a datapack a project here already uses opens that project's home; otherwise (a viewer share, a datapack that isn't a project datapack, or one Meko doesn't list yet) the palette says what to do. A link that's off or reset (Meko's 404) and a person the owner removed (`REMOVED_FROM_DATAPACK`) get their own messages. A link opened in Meko's web app instead is found by the invite sync as a share you accepted, as before. |
| An invite for you | A banner (and a notification) from `incoming-shares`: **Open as project** (creating the project accepts it), **Decline**, **Later**. A viewer invite gets **Accept** instead of Open as project. |
| A share you accepted, not yet a project here | A banner from `datapack_list`: **Open as project** and **Later** (a viewer share only explains, with **OK**). |

**Frequent people.** Every address an invite goes through for (from the Emails field or a
reviewer's row) is kept in the `share_contacts` table (`src/server/contacts.ts`): the address as
last typed, keyed in lower case, how many invites went through and when the latest did, and the
person's name once a datapack's member list (read by the Share dialog) shows it. Failed invites
and addresses left out because they already had access aren't counted. Migration 33 started the
table with the emails remembered for reviewers, once each. `GET /api/share-contacts` lists every
project's people, ranked by `shares / (1 + days since the latest / 30)` (often and lately first;
a share last month counts half as much as one today), the most recent first among equals; the
table keeps the 200 ranked highest. Under the Emails field, the Share dialog
(`ShareSuggestions.tsx`) shows up to six as chips once its people lists are in: **Frequent** while
nothing is being typed, and **Matches** for the entry being typed (`typedEntry()` in
`src/shared/emails.ts`). That entry is what follows the last comma or semicolon: an address with
no space or separator after it yet (so a half-typed `bob@x.c`, though it looks complete, is still
being typed), or the words of a name. An address matches when it holds the entry anywhere, a name
when the entry starts at one of its words (`Jane D` finds Jane Doe). People with access or a
pending invite, and addresses already in the field, aren't offered; until the lists are in (or
when they can't be read) there are no suggestions, since those people couldn't be left out. A
chip's name (the address beside it, but not on a phone) puts the address in the field in place of
the whole entry, all of a name's words included, followed by `, `; its **×** stops suggesting them
(`DELETE /api/share-contacts/:email`). The table's key lowers ASCII letters only, as SQLite's
`lower()` did when migration 33 seeded it. Saving to it never fails an invite that went through:
an error is logged. The list stays on this machine: it's not in the usage report or Meko. When it
can't be read, the dialog shows no suggestions.

**Sharing with a PR's reviewers.** Once a session has a PR, its Review & PR step has **Share with
reviewers** (on a phone, in the ⋯ sheet), while the project has a dedicated datapack and Meko
is on. While AgentK can't share (`MekoStatus.sharing` false), it opens the same Meko Cloud dialog as
the project home's Share. Otherwise it opens the Share dialog with a **Reviewers on the
project's open PRs** section above the email form: for each open PR of the project's running sessions (in Review & PR or Merge & Close),
each in its own session's repo, the people asked to review and the people who have reviewed,
without bots, the PR's author or you, and the teams asked (whose members are invited by email);
the session's own PR comes first, and a person on several PRs is listed once
(`GET /api/projects/:id/pr-reviewers`, from GitHub's REST API, which says which accounts are
bots). Each person's email is the one you shared with for their GitHub login before (kept in `kv`
as `reviewer-email:<login>`, written when a share names `login`), else the public email on their
GitHub profile (read once an hour at most), else typed. Each row has the access (Contributor by
default: a viewer can't open the datapack as a project of their own) and **Share**, and shows
**✓ has access** when the email is a member's or has a pending invite. The project home's
**Share** opens the same dialog. The reviewer then opens it as a project and starts a
[Review a PR](review-pr.md) session.

When AgentK creates a datapack for a new project, it sets the datapack's description (MCP
`datapack_update`) to `AgentK project "<name>" · repo <remote>`, so whoever it's shared
with can find the repo. The remote is the repo's `upstream` when it's a fork with a network
upstream, else its `origin`: people share the upstream repo, not your fork. Both are read with one
`git config --get-regexp` call, which the repo scan uses too. Only a network remote goes in: a local path (`/home/me/src/x.git`, `../x`)
is left out, and credentials in the URL (`https://user:token@…`) are removed, again when a
description is read. Descriptions written before the product's rename start `AgentK Local
project` and are read the same way. The update runs in the background, so it never holds up the project; a
failure is logged. Meko shows the
description only once an invite is accepted, so a pending invite is matched to a repo by name.

`InviteService` (`src/server/invites.ts`) keeps the shares in the `datapack_invites` table, one row
per datapack, synced at startup, every 5 minutes, and whenever the Open project dialog opens or you
tap **Refresh**. The dialog makes one call, `POST /api/invites/check`, which lists the datapacks
once and syncs with that same list. Only project datapacks count, and only ones no live project
here uses. States: `invited` (waiting in Meko), `shared` (accepted, not yet a project), `later`
(hidden here; an invite stays in Meko), `used` (a project here has used it; never offered again,
even after that project is closed, unless it's invited again), and `baseline`. The first sync
records the shares that already exist as `baseline`, without banners or notifications, so an
upgrade doesn't bring a burst of them; they can still be picked in the dialog. Invites always show.
An invite that disappears from Meko (revoked, or answered elsewhere) is removed; one accepted
elsewhere becomes `shared` without another notification. `AppState.invites` carries the `invited`
and `shared` ones. When accepting, AgentK sends its own Meko account id when it knows it (the
`user_id` on the datapacks it owns); Meko otherwise uses the caller's account.

One sync runs at a time; a sync asked for meanwhile waits for it. A sync reads Meko, then writes
the table in one transaction; when Accept, Decline, Later, a new project or a Meko account change
changed things in between, that read is stale, so it reads again rather than undo the change (a
list handed in by `/api/invites/check` carries a stamp taken before it was read, for the same
check). Our Meko account id is only taken from a read that wasn't stale. A `datapack_list` reply
of an unexpected shape is an error, not an empty list, so it can't make every share look gone; and
a share accepted here isn't taken for gone for 10 minutes while Meko's list catches up. An invite that was
already accepted (by an earlier try whose project then failed, or in Meko) isn't accepted again,
so **Accept and open project** can be retried. A change to the Meko URL or key may mean another
account: the rows other than `used` and the first-sync flag are dropped, so the new account's
existing shares are recorded quietly.
