# The menu bar app (macOS)

`Meko.app` puts the meko "m" in the macOS menu bar. Clicking it opens a window with your
datapacks, this Meko's and the linked Meko Cloud's, as one list sorted by name:

- **Filter** the list as you type, by name or description.
- **Copy** a datapack's name.
- **Open** one in Meko's web UI (or Meko Cloud's web app for one of Cloud's) with a double-click.
- **Search** one: the results are memories, knowledge and conversation turns, and clicking one
  copies its text.
- **Chat** with one: each question is searched, and the user's own Claude Code writes an answer
  from the results, citing them, as the web UI's Search answers do
  ([memory-extraction.md](memory-extraction.md#search-answers)). Chat needs answers on
  (`meko answers use claude`); each question costs one call of the user's quota. Each question is
  answered on its own, from its own search, and the chat lasts as long as the app runs. Clicking an
  answer or one of its sources copies it.

## Running it

`meko menubar start` writes a LaunchAgent, `com.yugabyte.meko.menubar`, that runs the app at each
login, and starts it now. Quit (in its window) quits it until the next login or
`meko menubar start`; a crash starts it again. `meko menubar stop` quits it and removes the job,
and `meko uninstall` removes the job too. The job runs the `Meko.app` beside the `meko` binary:
in a release that's `app/current/bin/Meko.app`, so after an update it runs the new release's app
without the job being written again. The plist is written only when it changes, because macOS's
Background Task Management shows a "Background Items Added" notification for each rewritten one;
Login Items name the job by its program, `meko-menubar`. The job passes the app `--meko <meko
binary> --data-dir <data folder>`; the app's own errors go to `logs/menubar.log`.

## How it reaches Meko

The app runs `meko token --json` (with the job's `--data-dir`) for the agents' listener's address
(`url`, the MCP URL, plain http on 127.0.0.1: the REST API is at `/api/v1` beside it) and the client
token, which that listener takes. It never calls the web listener, which takes no token and is off
after `meko ui stop`, or HTTPS with its own certificate under remote access; `web_url` is only where
it opens pages (while the web UI is off it says to run `meko ui start`). It calls
`GET /api/v1/datapacks` for the list,
`POST /api/v1/datapacks/:id/search` for a search (with `agent_id` `meko_menubar`, so its searches
show as its own in the call log and Context Gaps), and `POST /api/v1/datapacks/:id/answer` for a
chat's answer, which takes Meko Cloud's datapacks too ([api.md](api.md#rest-at-apiv1)). It reads the
address and token again after a 401 (Meko was set up again, with a new token) or a failed
connection, which it reports as Meko not answering at that address, with `meko status` to run. Started
without arguments (opened from Finder), it uses the `meko` beside it, else `~/.local/bin/meko`.

## Building it

`make menubar` runs `menubar/build.sh`, which builds `bin/Meko.app` with `swiftc` from the Command
Line Tools: no Xcode project. It's one SwiftUI file (`MenuBarExtra` in its window style, which takes
text input, unlike the menu style), for macOS 13 or newer, signed ad hoc as `meko` is unsigned.
Those tools have no SwiftUI macro plugin, so the app can't use macros such as `@State` (a macro in
the macOS 26 SDK): its view state lives in the `ObservableObject` model. The release's macOS job
runs `make menubar` before packaging, `package-release.sh` puts the app in the darwin tarball's
`bin/`, and `smoke-release.sh` checks its signature. CI's macOS backend job builds it too.
