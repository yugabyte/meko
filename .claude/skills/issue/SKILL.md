---
name: issue
description: Write, file or edit a GitHub issue in the meko repo (yugabyte/meko). Use whenever the user asks to file, open, create, write or draft an issue, report a bug, request a feature, turn a finding or a conversation into an issue, add a follow-up to an issue, or change an issue's title or body here. Issues use the same sections as the repo's issue forms, its type and labels, and nothing that mustn't be public.
---

# Issues in meko

The repo's issue forms (`.github/ISSUE_TEMPLATE/`) give issues filed on the web their sections,
type and label. `gh issue create` doesn't fill in forms, so an agent writes the same sections
itself and sets the type and labels by hand. Issues read the same however they were filed.

## Before writing

Search for one that already covers it, open and closed:

```sh
gh issue list --search "<a few words>" --state all --limit 20
```

If one does, show it to the user and offer to comment on it or update it instead.

## Kind, type and labels

| Kind | When | `--type` | Labels |
|---|---|---|---|
| Bug | Something doesn't work as it should | `Bug` | `bug` |
| Feature | Something new, or a change in behavior users will see | `Feature` | `enhancement` |
| Task | Planned work, a chore or a checklist (a release step, a setting, a cleanup) | `Task` | none |

Add a product label for each product the issue is about: `meko` (Meko, `src/meko`), `agentk`
(`src/extensions/agentk`), `present` (`src/extensions/present`). An issue about only the shared root
(CI, `scripts/`, root docs) takes none. Forms can't set these, so issues filed on the web get
theirs when someone triages them; add a missing one when you come across it.

## Title

A plain sentence: what's wrong (bug), what should become possible (feature), or what to do
(task). Name the product when the sentence doesn't make it clear. Not the
`<type>(<component>): …` format, which is for commits and PRs. Keep it under about 90
characters.

- `Kode update can leave existing installations without a reachable web UI`
- `Verify release provenance in Meko's updater, pinned to the release workflow on main`

## Body

Use the form's headings, as `##` headings, in this order. Leave out a section you have nothing
for rather than writing "N/A".

- **Bug** (`bug.yml`): **Product**, **Version** (the version lines of `meko status`),
  **What happens**, **Steps to reproduce**, **What you expected**, **Logs or output**,
  **Environment**.
- **Feature** (`feature.yml`): **Product**, **Problem**, **Proposal**, **Alternatives you
  considered**, **Done when**.
- **Task** (`task.yml`): **Product**, **Context**, **Plan** (a `- [ ]` checklist), **Done when**.

When you know more than a reporter would, add it after the form's sections (see #268, #296 and
#314):

- **Why**: the cause, with code pointers: the file and the function or symbol
  (`src/extensions/agentk/src/server/harness/opencode.ts`, `normalizeOpenCode`). Line numbers go stale.
- **Workaround**, **Tests** (what would prove it fixed), **Out of scope**, **Open question**.

Write plain, specific sentences. "Done when" items are outcomes someone can check. Logs and
command output go in fenced blocks, trimmed to the lines that matter.

## Never in an issue

The repo is public: no internal hostnames or URLs, mount paths, tokens, API keys or session
cookies, and no personal data (the repo root's AGENTS.md has the rule). That means no customer
or company names, no one's name, email address, phone number, user name or handle, account ID
or IP address, and nothing a user wrote or stored (prompts, transcripts, memories, datapack
contents). Say "a user" or "the reporter" instead, and use made-up values where an example
needs one (`alice@example.com`, `github.com/acme/demo-repo`). Write `~` for a home folder.
`meko status` and many logs print the web UI's hostname, the data folder's path, and user,
datapack and project names: cut or replace them. Screenshots need the same care.

A vulnerability is never an issue: it's reported privately, as the repo root's `SECURITY.md`
says. The issue chooser's **Report a vulnerability** link (`.github/ISSUE_TEMPLATE/config.yml`)
points there.

## Filing

Show the user the title, labels, type and body, and file once they say so. If they've already
asked for it to be filed ("file an issue for this"), file it without asking again.

Write the body to a file in your scratch directory, then:

```sh
gh issue create --repo yugabyte/meko --title "<title>" --body-file <file> \
  --type Bug --label bug --label agentk
```

If the local `gh` is too old to know `--type`, file without it and tell the user to set the type
on the issue's page. Give the user the issue's URL.

- Refer to issues and PRs as `#N`. A sub-task of a bigger issue goes in as
  `gh issue create … --parent <N>`.
- A follow-up of a checklist issue (such as #312) is added to that issue's checklist too: see
  Editing.

## Editing

Read the issue first, change only what was asked, and keep the rest of the author's text and
the state of every checkbox:

```sh
gh issue view <N> --repo yugabyte/meko --json title,body,labels --jq .body > <file>
# edit <file>
gh issue edit <N> --repo yugabyte/meko --body-file <file>
```

`gh issue edit` also takes `--title`, `--add-label`, `--remove-label`, `--type` and `--parent`. To add something new,
such as a finding, prefer `gh issue comment <N> --body-file <file>` over rewriting the body,
unless the user asks for the body to change.
