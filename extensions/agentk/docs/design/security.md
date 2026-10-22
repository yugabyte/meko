# Login and security

Part of the [AgentK design](../design.md).

The terminal is a shell running as your Unix user, so the web login protects the same thing as
your SSH key. Each install serves exactly one owner. Sharing the app means other people install
their own copy.

- **Local or remote access.** The installer asks which you want. *Local* listens on 127.0.0.1
  only, over plain HTTP with no login: a request needs no cookie when the server listens only on
  loopback, the connection comes from loopback, and its `Host` header is `localhost`, `127.0.0.1`
  or `[::1]` (so a DNS-rebound page, which sends its own host name, gets nothing), with no
  `Forwarded`/`X-Forwarded-*`/`X-Real-IP` header from a proxy, and the browser doesn't mark it as
  sent by another site (`Sec-Fetch-Site` other than `same-origin` or `none`, or an `Origin` that
  isn't this one). That last check does the SameSite=Strict cookie's job: an `<img>` on another
  site gets a 401, not an answer it could time. An existing login is kept for a later switch to
  remote; the server's startup log says it isn't needed meanwhile. *Remote*
  listens on every address (`0.0.0.0`), over HTTPS with a login, and then the login is required
  even from localhost, so an SSH tunnel or a reverse proxy on the host can't skip it. The default
  is remote over SSH or on Linux with no display, and local otherwise. Local access trusts
  everything that can connect to 127.0.0.1, every user of the computer and any reverse proxy on it
  included: on a shared host, or behind a proxy, choose remote.
- **First run.** With remote access, the installer prints a one-time setup link:
  `https://host:7777/setup?token=…`. You open it, choose a username (up to 64 letters, digits,
  `.`, `@`, `-` or `_`: most email addresses work, but not ones with `+`) and password, and can
  optionally enrol TOTP. The token stops working after it is used. With local access it prints
  the address instead; `agent-k-local setup-link` still makes a login for a later switch to remote.
- **Forgot password.** The sign-in screen's **Forgot password?** says how to get back in: run
  `agent-k-local setup-link` on the host and open the link it prints. There is no email or
  self-service reset in the browser, on purpose: the one login guards a shell on the host, so a
  reset needs someone who can already run commands there. While a link is outstanding the setup
  screen shows (`AuthStatus.reset` says it replaces an existing login, so it reads **Reset your
  login**); using it replaces the username and password, turns off TOTP and signs out every
  browser. A valid link works even while sign-in is rate limited, so a locked-out owner can use
  it at once. A browser that is still signed in keeps working until then and doesn't see the setup
  screen. Projects and sessions are untouched.
- **Sessions.** Passwords are hashed with scrypt. After login you get an `HttpOnly`,
  `SameSite=Strict` cookie (`Secure` over HTTPS) that lasts 14 days, renewed as you use the app.
  Changing the password signs out every other browser. Access checks key on the route a request
  matched, so an encoded path (`/%61pi/…`) gets the same checks as `/api/…`. The API only takes
  JSON bodies, so a plain HTML form can't post to it; the routes that take a file (pasted images and files,
  knowledge-base uploads) parse `image/*` or `application/octet-stream`, types a form can't send,
  each only in its own scope. Every WebSocket upgrade checks the cookie (or
  local access) and refuses a cross-origin `Origin` header; open sockets close (code 4401) when their login is signed
  out, and the server pings them every 30 seconds to drop ones whose network went away. The page
  checks its events socket too, since a phone often never hears that the server dropped it: after
  30 seconds without a message, and whenever the page is shown again or comes back online, it sends
  a `ping`, and opens a new socket (which starts with the current state) if no `pong` comes back
  within 5 seconds, or at once if the socket is already closing. A new socket gets 10 seconds to
  open and 30 more for its first state. Each of these deadlines that passes doubles the next ones
  (up to 8 times), so a slow link whose snapshots take longer still gets through; a quick answer
  to a ping sets them back (`src/web/events.ts`). A browser
  that falls behind doesn't make the server buffer without limit: its terminal stops reading from
  the session host until it catches up (the host skips a viewer that stops reading and sends it the
  screen afresh once it does; Claude never waits on a viewer), and it skips state snapshots
  until it has drained, then gets the current one.
- **TLS is mandatory.** Any address other than localhost requires HTTPS. You can point it at your
  own certificate from the corp CA, or let it generate a self-signed pair; `agent-k-local status`
  shows its fingerprint so you can trust it once on your laptop. The self-signed pair lasts 825 days and is made again
  at startup once it's within 30 days of expiring (trust the new fingerprint then). No HSTS header
  is sent: it would apply to every port of the host, including your other dev servers.
- **Guard rails.** Failed logins are rate-limited: 5 per 15 minutes from one address (one /64 for
  IPv6, since a client usually holds a whole one), and 50 across all clients, after which everyone
  waits (`agent-k-local setup-link` on the host still gets you in). A wrong username takes as long
  to refuse as a wrong password. A TOTP code works once. There is an
  audit log of logins, failed logins, account changes and Meko settings changes. The Meko key is
  write-only from the browser's side: no response carries it, and a saved key goes only where it
  already goes (the MCP server and API URL in use) or to Meko Cloud. A change on the Meko tab that
  would send it to another server (the MCP, web app or API URL) is refused until the key is typed
  again, before anything is sent, with Meko on or off (a URL saved while it's off would get the key
  once it's back on), so someone with a stolen login can't point AgentK at their own server and
  read the key off it. Hooks use the local unix socket with a per-session
  token, never the network port. The Design-stage edit guard asks the server over the same socket;
  if it gets no answer it lets the edit go ahead, so Claude is never stuck.
- **The page's policy.** Every app page carries a Content-Security-Policy (`appCsp()` in
  `server.ts`) that keeps out images from anywhere but the server, `data:`, `blob:` and GitHub (a
  remote image in an outsider's review, shown in the held reviews sheet, would tell its author who
  read it and when), connections to other servers, plugins, a `<base>` or form pointing elsewhere,
  and framing. It has no `script-src`: the design panel shows an Artifact in a `srcdoc` frame, which
  takes the page's policy on top of its own, and Artifacts run inline scripts and scripts from two
  CDNs (`design-frame.ts`). Markdown is sanitized (DOMPurify) and the frame runs without
  `allow-same-origin`, so scripts are kept out by those instead.
- **Files on disk.** `state.db` (the password hash, the TOTP secret, hook tokens) is owner-only
  (0600) whatever folder it's in, and so are its `-wal` and `-shm` files, which SQLite gives the
  database's mode, and so are the snapshots the updater takes before an update
  (`state.db.bak-<version>`); ones made before this are fixed when the server opens the database.
  A folder AgentK creates for it is 0700 (so is `$XDG_STATE_HOME/agent-k-local`); an existing one,
  such as AgentK's home on an older install or a `[state] db_path` folder, keeps its mode.

## Code from public repos

Code on a private or internal GitHub repo is trusted like your own: only people with access can
push to it or open a PR on it (a private repo's forks are private too). Code on a **public** repo's
PR head isn't: anyone can open a PR. A session works on such a head when it reviews a PR (Review a
PR) or is a task started on a PR, or on an existing branch whose open PR it took
(`prHead()` in `src/server/headtrust.ts`). Before its first prompt and at every launch, AgentK
reads the PR's repo's visibility (`gh api repos/<owner>/<name>`, kept 10 minutes, so a repo made
public later is noticed), and a public repo, or one gh can't read, marks the session untrusted
(`sessions.untrusted`, with a line in its log saying why). An untrusted session runs without the
repo's own config, which would otherwise run the PR author's commands as you with no prompt (AgentK
answers Claude Code's folder-trust question for worktrees):

- Claude Code starts with `--setting-sources user`: none of the repo's `.claude/settings*.json`
  (hooks, permissions, `env`, plugins, status line) or `.mcp.json` servers load. AgentK's own
  `--settings`, its mod and your user-level MCP servers (Meko's included) still do. Checked with
  Claude Code 2.1.296: a project `SessionStart` hook and a `.mcp.json` server both ran without the
  flag, and neither did with it.
- The status line relay runs your user status line, never the worktree's.
- The repo's `.agent-k/stages/` prompts aren't used; AgentK's (or yours from Settings) are. Until a
  session's head has been checked, it's treated as untrusted here.
- Kiro has no way to leave a workspace's config out, and uses a workspace agent named `agentk`
  over AgentK's (checked with kiro-cli 2.32). An untrusted session on Kiro doesn't start while the
  worktree has one, or workspace MCP servers (`.kiro/settings/mcp.json`); the error says to review
  it on Claude Code.

`CLAUDE.md`, `AGENTS.md` and the code itself are still what Claude reads: the review prompts call
the PR's content untrusted text, not instructions. A task started from a public repo's issue whose
author has no role on the repo (not OWNER, MEMBER or COLLABORATOR) tells Claude to treat the issue
as a report to verify, and not to run commands, install anything or open links it asks for
(`issuePrompt()` in `ghlinks.ts`). And a session takes only a PR whose head is where its branch is
pushed (`origin`, or the fork's remote it pushes to) as its PR, so a stranger's PR from a fork's
branch of the same name is never followed or merged (`pickPr()` in `github.ts`). When that remote's
URL doesn't name its GitHub repo (an SSH host alias, a GitHub Enterprise host), any PR opened since
the session started still counts, as before. A session's head is checked again whenever it takes
a PR, and one with no PR head isn't treated as checked once it has one.

*Later: passkeys (WebAuthn), and single sign-on with Google Workspace or another OIDC provider.*
