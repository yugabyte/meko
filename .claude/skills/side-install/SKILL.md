---
name: side-install
description: Run Kode from this checkout as a second, side install beside the installed Kode (npm run dev with a fresh home, the right claude, the user's Meko, and the Kode tour starting by itself), or take one down and clean up what it made. Use whenever the user asks to side install Kode, install Kode on the side, run or try this branch as a second Kode, dry-run the tour on a fresh home, or take down, stop or remove a side install.
---

# Kode side installs

A side install is Kode's `npm run dev` (in `src/extensions/agentk`) with `KODE_DEV_HOME` set to a
fresh home. It has its own database, session host and sessions, and serves its UI through Vite at
`127.0.0.1:5173`, while the installed Kode keeps running. It shares the user's Meko and repos
folder with the installed Kode, so the tour makes real datapacks and a real demo repo: taking a
side install down removes them.

Two tasks: [Start](#start) and [Take down](#take-down). Run every command from the agentk folder
of the checkout the user wants to try (`src/extensions/agentk` under the repo root). Never put
this machine's paths, host names or user names in anything the repo keeps.

## Start

1. **Dependencies.** Node 22.13 or later (`node --version`; otherwise `nvm which 22`, or ask the
   user where it is). `npm ci` if `node_modules` is missing (node-pty needs the C++20 compiler
   named under Commands in `src/extensions/agentk/AGENTS.md`).
2. **Ask only what the defaults don't cover.** The defaults: the Kode tour starts by itself, and
   Kiro and OpenCode are off. If the user asks for Kiro, warn them first: the dev server then
   rewrites `~/.kiro/agents/kode.json` (and `kode-trusted.json`, or deletes it) for the side
   home, which breaks the installed Kode's Kiro sessions until teardown restores them. OpenCode
   has no such effect.
3. **Set up the home:**

   ```sh
   agentk=$PWD
   node_bin=$(dirname "$(command -v node)")      # the Node 22 from step 1
   claude_cmd=$(command -v claude)               # an absolute path; keep its symlinks
   "$claude_cmd" --version </dev/null            # must answer
   meko_home=${MEKO_HOME:-$HOME/.meko}           # must exist
   home=$(mktemp -d "${TMPDIR:-/tmp}/kode-side.XXXXXX")
   cat >"$home/config.toml" <<EOF
   [meko]
   home = "$meko_home"

   [claude]
   command = "$claude_cmd"

   [kiro]
   enabled = false
   trust_tools = true

   [opencode]
   enabled = false

   [welcome]
   tour = "agentk-101"
   EOF
   printf 'agentk=%s\nmeko_home=%s\nkiro=off\n' "$agentk" "$meko_home" >"$home/side.env"
   ```

   `trust_tools = true` matters even with Kiro off: at every start, Kode deletes
   `~/.kiro/agents/kode-trusted.json` when `trust_tools` is off, before it checks whether Kiro is
   on (`prepare` in `src/server/harness/kiro.ts`). The installed Kode may be using that file. With
   Kiro off, Kode then stops there and leaves `~/.kiro` alone.

   `command -v claude` must print a path starting with `/` (not an alias or function). Don't
   resolve its symlinks: Claude Code's installer links it to a versioned binary that an update
   deletes. Leave out `[welcome]` when the user doesn't want the tour; set `enabled = true` for
   an agent the user asked for. For Kiro, also set `trust_tools` to the installed Kode's value
   (`[kiro]` in `"$meko_home"/extensions/kode/config.toml`; `false` when it's not there) and write
   `kiro=on` in `side.env` instead of `kiro=off`.
4. **Run it** from a clean environment, in a session and process group of its own:

   ```sh
   pgid=$(env -i HOME="$HOME" USER="$USER" SHELL="$SHELL" TERM=xterm-256color \
     PATH="$node_bin:/usr/local/bin:/usr/bin:/bin" KODE_DEV_HOME="$home" \
     node -e '
       const log = require("fs").openSync(process.argv[1], "a");
       const c = require("child_process").spawn("npm", ["run", "dev"], { detached: true, stdio: ["ignore", log, log] });
       c.unref(); console.log(c.pid);' "$home/dev.log")
   echo "pgid=$pgid" >>"$home/side.env"
   ```

   `env -i` keeps the agent session's `MEKO_*`, `KODE_*` and nvm state out: started inside an
   agent session, the dev server takes PATH from a login shell that may not find `claude`, and
   sessions then fall back to another agent. `detached` makes npm the leader of a new process
   group, so it outlives this session and stops as one (npm, concurrently, tsx, Vite).
5. **Wait until it's up** (up to a minute): Vite's `Local:` line in `$home/dev.log` gives the
   address (it takes 5174 and up when 5173 is busy), then
   `curl -s -o /dev/null -w '%{http_code}' <address>healthz` answers 200. Vite's
   `http proxy error … ENOENT …/web.sock` lines are normal until the server has made its socket.
   If it doesn't answer, show the end of `dev.log` and take the install down.
6. **Tell the user** in a few lines: the address; that the tour starts by itself on first load
   (it builds a demo repo in the repos folder, `~/code` by default, and two datapacks in their
   Meko); and, when they're on another computer, that Vite listens on 127.0.0.1 only, so they
   need `ssh -L <port>:127.0.0.1:<port> <this host>`. The server's log line "browsers open it at
   https://…/ext/kode/" is Meko's address for the installed Kode, not this one.

## Take down

1. **Which install.** Each one's `side.env` is at `${TMPDIR:-/tmp}/kode-side.*/side.env`. One:
   use it. More than one: ask which. None: say so and stop. Read its values without running the
   file as shell (a path may hold spaces), then `cd "$agentk"`:

   ```sh
   val() { sed -n "s/^$1=//p" "$home/side.env" | head -n 1; }
   agentk=$(val agentk); meko_home=$(val meko_home); pgid=$(val pgid); kiro=$(val kiro)
   ```

2. **Find what it made** before stopping anything, from its own database, never by name: the
   tour's demo folder is `kode-demo`, `kode-demo-2`, … (the first free one, and the installed Kode
   may own another), and its datapacks get a number or random suffix when their name is taken.

   ```sh
   node --disable-warning=ExperimentalWarning -e '
     const db = new (require("node:sqlite").DatabaseSync)(process.argv[1], { readOnly: true });
     for (const r of db.prepare(`SELECT p.repo_path, p.workshop, d.datapack_id, d.name FROM projects p
         LEFT JOIN project_datapacks d ON d.project_id = p.id AND d.role = ?`).all("dedicated"))
       console.log([r.workshop ? "tour" : "project", r.repo_path, r.datapack_id ?? "", r.name ?? ""].join("\t"));
     for (const { a } of db.prepare("SELECT meko_agent_id AS a FROM sessions WHERE meko_agent_id IS NOT NULL").all())
       console.log(["pin", "pin-" + (a.trim().replace(/[^A-Za-z0-9._-]+/g, "_").replace(/^_+|_+$/g, "") || "meko_agent") + ".json"].join("\t"));' "$home/state.db"
   ```

   - A `tour` row's repo is the demo repo. Its datapacks are the ones its `MEKO.md` lists (Kode
     writes it into `main` when it builds the demo; this prints `<id> <name>` for each), plus the
     row's own dedicated datapack:

     ```sh
     git -C "<demo repo>" show main:MEKO.md | sed -n 's/^| `\([^`]*\)` | `\([^`]*\)` |.*/\2 \1/p'
     ```

   - A `project` row is a repo the user opened in the side install. Leave it and its datapack
     alone; only its worktree list gets pruned.
   - A `pin` row is the Meko pin file of one of the side install's sessions, in
     `${MEKO_WATERMARK_DIR:-$HOME/.claude/meko-capture}`. Only the side server would ever prune
     it, and no other session uses its agent id.
3. **Ask once**, listing the folders and the datapacks (name and id) that will be removed. Stop
   there if the user says no.
4. **Stop it**, the dev server first (the session host refuses to stop while a server holds the
   home's lock):

   ```sh
   kill -TERM -"$pgid"
   ps -eo pid=,pgid= | awk -v g="$pgid" '$2 == g'   # wait until this prints nothing
   KODE_HOME="$home" npx tsx src/server/cli.ts host stop
   ```

   "No session host is running" is fine. If `host stop` says a server is running, the kill
   missed it: find it with `ps -eo pid=,args= | grep -F "$home"`.
5. **Delete the tour's datapacks:**
   `MEKO_HOME="$meko_home" meko-cli delete_datapack <id> --force` for each. One already gone is
   fine; report any other failure.
6. **Remove the files**, each only after checking it's the path from step 2 (the demo repo) or
   matches `kode-side.*` (the home), in this order:
   - `rm -rf` the demo repo;
   - `rm -f` each `pin` row's file in the capture folder;
   - with `kiro=on`, restore the installed Kode's Kiro agents. For each of `kode` and
     `kode-trusted`: copy `"$meko_home"/extensions/kode/run/kiro-<name>.json` to
     `${KIRO_HOME:-$HOME/.kiro}/agents/<name>.json` when the installed Kode has that copy, and
     otherwise remove `agents/<name>.json` (the side install wrote it);
   - `rm -rf "$home"`;
   - then `git -C <repo> worktree prune` in each `project` row's repo. The side install's
     worktrees were in its home, and git prunes an entry only once its folder is gone; until then
     it keeps their branches checked out.
7. **Report** in a line or two what was removed and anything left (a datapack that failed, a
   process still running, branches Kode made in a `project` repo).

When the home is already gone (a reboot can clear the temp folder), there's nothing to tie the
leftovers to the install: delete nothing, and tell the user what to look for by hand (a
`kode-demo*` folder in the repos folder, `adlc/repo/kode-demo*/gotchas` and
`adlc/proj/kode-demo*/…` datapacks in `meko-cli datapacks`), and that a running session host
shows in `ps` with the old home's path.
