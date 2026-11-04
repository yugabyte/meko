# Access and security

Part of the [AgentK design](../design.md).

The terminal is a shell running as your Unix user, so whoever can open AgentK can do what your SSH
key can. Each install serves exactly one owner. Sharing the app means other people install their
own copy.

- **Meko is the front door.** AgentK doesn't listen on the network and has no login of its own,
  and Meko has none either.
  Its server listens only on a unix socket (`Paths.webSock`, `web.sock` in its home, or in its
  private socket folder in the temp dir when that path is too long), in a folder only you can open,
  and lets in every request that arrives there. The Meko on this computer serves it to browsers at
  `/ext/agentk/` on its web listener (`:7777`), with the path stripped, after its own access
  check ([Meko's web.md](../../../../meko/docs/web.md)):
  - **Local access** (Meko's `[web] access = "local"`): Meko listens on 127.0.0.1 over plain HTTP,
    and a request from this computer needs no login: a loopback peer, a `Host` of `localhost`,
    `127.0.0.1` or `[::1]` (so a DNS-rebound page, which sends its own host name, gets nothing), no
    `Forwarded`/`X-Forwarded-*`/`X-Real-IP` header from a proxy, and nothing saying another site sent
    it (`Sec-Fetch-Site` other than `same-origin` or `none`, or an `Origin` that isn't this one).
    It trusts everything that can connect to 127.0.0.1, every user of the computer and any tunnel or
    proxy on it included: on a shared host, choose remote.
  - **Remote access** (`"remote"`): Meko listens on every address over HTTPS, with its own
    certificate or a self-signed one (`meko status` shows its fingerprint), and lets in every
    request another site's page didn't send. Anyone who can reach the port can open AgentK, and its
    terminals: choose it only where that's who you mean. `meko ui stop` on the host closes the port
    (AgentK's page with it) until `meko ui start`, and closes the terminals and event sockets Meko
    proxied.
- **What reaches AgentK.** Meko passes the browser's `Host` and `Origin` on as they are, drops the
  `Cookie` header, and adds no forwarding headers. AgentK refuses a write (any `/api/*` request but GET) or a
  WebSocket upgrade whose `Origin` isn't the page's own (403, or close code 1008), so another site's
  page can't act through a browser Meko let in. The API only takes JSON bodies, so a plain HTML form
  can't post to it; the routes that take a file (pasted images and files, knowledge-base uploads)
  parse `image/*` or `application/octet-stream`, types a form can't send, each only in its own
  scope. The server pings every socket every 30 seconds to drop ones whose network went away. The
  page checks its events socket too, since a phone often never hears that the server dropped it:
  after 30 seconds without a message, and whenever the page is shown again or comes back online, it
  sends a `ping`, and opens a new socket (which starts with the current state) if no `pong` comes
  back within 5 seconds, or at once if the socket is already closing. A new socket gets 10 seconds to
  open and 30 more for its first state. Each of these deadlines that passes doubles the next ones
  (up to 8 times), so a slow link whose snapshots take longer still gets through; a quick answer to
  a ping sets them back (`src/web/events.ts`). A browser that falls behind doesn't make the server
  buffer without limit: its terminal stops reading from the session host until it catches up (the
  host skips a viewer that stops reading and sends it the screen afresh once it does; Claude never
  waits on a viewer), and it skips state snapshots until it has drained, then gets the current one.
  When Meko refuses a request (a 401, or a socket it refused), the page asks Meko's
  `/api/v1/auth/status`, and goes to Meko's sign-in only when it says remote access and not signed
  in, which only a Meko with a login says; otherwise it keeps retrying.
- **The handoff.** At each of its starts Meko runs `me_koder _handoff --json` (`handoff.ts`), which
  says where the socket is and, for an install that served browsers itself (on port 7777, over
  HTTPS with its own login), its old `[server]` bind and port, its certificate (self-signed in
  `certs/`, or `[tls]`'s) and its login (`owner` in `state.db`). Meko takes the port, access and
  certificate over once, copying what it doesn't have yet, so the same URL and certificate
  fingerprint keep working; it ignores the login, having none. Nothing else in AgentK reads them.
  `state.db` is read read-only, the one read outside the server's lease, and only on the host whose
  server holds it: `_handoff` refuses while the home's server runs on another host, whose socket
  this host can't reach anyway. On
  shutdown the server removes `web.sock` only while it's still the file it bound (`socketId`), as it
  does the hook socket: after a lost lease another server may have bound its own.
- **Guard rails.** AgentK has no route that sets or shows a Meko key: Meko owns the Meko Cloud
  link and keeps its key, and AgentK reaches Meko Cloud through the Meko on this computer with that
  Meko's token (an older Meko handed out the key in `meko token --json`, which AgentK then holds in
  memory and never writes to `config.toml`). A key of its own from before is handed to Meko on
  stdin, once, then never read again, though it stays in `config.toml` for a rollback. No
  response carries it. Meko's own page refuses to send a saved key to another server (the MCP, web
  app or API URL) until it's typed again, so someone who reaches the page can't point it at their
  own server and read the key off it. `POST /_meko/changed` only makes AgentK read Meko again, and
  refuses another origin. The Meko on this computer's client token, which AgentK uses when
  Meko has no Meko Cloud link, is read from Meko (`meko token --json`), held in memory, never written to
  `config.toml`, and sent only to that Meko: no configured MCP URL takes it, and a web app or API URL
  from config or the environment is used with it only on that Meko's own origin. Hooks use their
  own unix socket with a per-session token, never the web socket. The Design-stage edit guard asks the server over the same socket;
  if it gets no answer it lets the edit go ahead, so Claude is never stuck.
- **The page's policy.** Every app page carries a Content-Security-Policy (`appCsp()` in
  `server.ts`) that keeps out images from anywhere but the server, `data:`, `blob:` and GitHub (a
  remote image in an outsider's review, shown in the held reviews sheet, would tell its author who
  read it and when), connections to other servers, plugins, a `<base>` or form pointing elsewhere,
  and framing. It has no `script-src`: the design panel shows an Artifact in a `srcdoc` frame, which
  takes the page's policy on top of its own, and Artifacts run inline scripts and scripts from two
  CDNs (`design-frame.ts`). Markdown is sanitized (DOMPurify) and the frame runs without
  `allow-same-origin`, so scripts are kept out by those instead.
- **Files on disk.** `state.db` (hook tokens, and an older install's password hash and TOTP secret,
  which Meko's handoff reads and ignores) is owner-only
  (0600) whatever folder it's in, and so are its `-wal` and `-shm` files, which SQLite gives the
  database's mode, and so are the snapshots a version takes of it before migrating it after an update
  (`state.db.bak-<version>`); ones made before this are fixed when the server opens the database.
  A folder AgentK creates for it is 0700 (so are its home, and Meko's data folder and its
  `extensions/` when AgentK makes them first); an existing one, such as AgentK's home on an older
  install or a `[state] db_path` folder of one, keeps its mode. In `/var/tmp`, Meko's data folder
  must be a real directory owned by you that others can't write to (`ensurePrivateDir`, and
  `private_dir` in the installer), or AgentK stops.

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
- OpenCode starts with `OPENCODE_DISABLE_PROJECT_CONFIG=1`, which leaves out the worktree's
  `opencode.json(c)` and `.opencode/` folders: its plugins (code OpenCode would run), agents,
  commands and MCP servers (checked against OpenCode 1.19.4's and 2.1.4's source). AgentK's own plugin and
  config still load, from the session's environment.
- Kiro has no way to leave a workspace's config out, and uses a workspace agent named `agentk`
  (or `agentk-trusted`, a trusted launch's agent on Kiro 3.0) over AgentK's (checked with kiro-cli 2.32). An untrusted session on Kiro doesn't start while the
  worktree has one, or workspace MCP servers (`.kiro/settings/mcp.json`); the error says to review
  it on Claude Code.

- [AgentK's session tools](tools.md), which start and move other sessions, refuse every call from an
  untrusted session (`repoConfigTrusted()`: one not checked yet counts), and a Claude Code session launches without them;
  Kiro's shared agent file has them, and the route refuses them.

Every session that has the tools can act on the others only through the same checks as the UI
(`runActions()` in `src/server/actions.ts`): no Open PR on its own session (that stays the user's
gate), the tools that change something go through the harness's own permission ask, and each action
leaves a `tools` event on the calling session and on the one it changed.

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
