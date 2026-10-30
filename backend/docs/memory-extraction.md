# Meko backend: memory extraction

Meko pulls memories out of conversation turns through the user's own AI tool's CLI, run headless:
Claude Code today (`claude -p`), Codex and Cursor later. It's off until Settings or `meko
memory-extractor use claude` turns it on.

## How it works

1. **Queue.** While the extractor is on, `conversation_add_message` queues each new turn
   (`extraction_queue`, in the transaction that stores it), unless its datapack opted out.
2. **Run.** Every interval (5 minutes by default, at least 1), and at once on `meko memory-extractor
   run` or a settings change, a run takes up to 50 conversations with waiting turns, oldest waiting
   first, each until its queue is empty: one call per 30 of its turns. Any more conversations wait for
   the next run.
3. **Prepare.** Turns with nothing to remember (under 12 characters: "ok", "thanks") leave the queue
   without a call. The rest go in one call per conversation: today's date, the 20 memories of the
   datapack nearest the turns (the embedder finds them), the 4 turns before as context, and the new
   turns numbered, the user's text cut to its first and last characters past 4,000, each reply to 300.
4. **Extract.** The CLI returns `{"memory": [{"text", "turn"}]}`, its JSON schema enforced.
5. **Save.** A fact within 0.9 cosine of any of the 5 memories nearest it is dropped; the rest are
   added through the memory engine as the turn's conversation's agent, with `source` `extract` (the
   memories table's provenance column; `add` for every other memory) and `meko_message_id`, the turn,
   in their metadata. The turns leave the queue.

A failed call (not signed in, a timeout, a reply without the JSON) leaves its turns queued with
`attempts` and `last_error`, and the run goes on to the next conversation; 3 failing in a row end it,
the harness itself being likely down. A turn that failed 5 times leaves the queue. The last error is
shown by `meko memory-extractor status`, the Status page and `GET /settings/memory-extractor`. It
says what failed and never what the harness wrote, which can quote the turns: the harness's own error
(such as "Not logged in"), its exit status, or a fixed reason.

## Decisions

| Decision | Why |
|---|---|
| **The user's own CLI, not a bundled or hosted model** | Meko local runs beside a strong LLM already: the user's agent. No model to download, no key to keep, and the quota is the user's choice. A small local model extracts poorly from coding conversations. |
| **Meko Cloud's behavior: the user's messages only, add only** | Facts come from what the user said, so the agent's general-knowledge answers don't become memories; replies are context. Adding only, with the nearest existing memories shown, needs no update or delete step. |
| **A short prompt, with profiles and the datapack's own instructions** (`extractprompt.go`, written for Meko) | Every call pays for the prompt, so it holds only the rules that decide what's kept. A datapack picks `general` or `coding` (rules and examples for coding conversations: keep conventions, stack, decisions, recurring fixes; skip transient code, pasted files and logs) and may add instructions (`meko memory-extractor datapack <name> --profile coding --instructions "..."`). |
| **A fixed interval, one call per conversation** | Predictable, and a call covers up to 30 of a conversation's waiting turns, so its fixed cost is shared. |
| **Claude Code runs with no tools, MCP servers, slash commands or saved session, and only the local settings of an empty folder** | Turns are untrusted text. The empty folder's settings keep the user's hooks (Meko's own capture among them) from running inside the call; `--bare` would too, but skips the saved login. `--system-prompt` replaces Claude Code's own (about 4k tokens). |
| **Setting it up is the owner's: the CLI, or the web UI's Settings** | It makes the server run a program, so `PUT /settings/memory-extractor` refuses the client token (agents) and takes only the owner: the admin token, a signed-in session or local access. The CLI sends the absolute path `meko memory-extractor use` finds on the user's PATH (the login service's own may not have it); the web UI sends no path, and the server finds the harness as it finds the agents it connects (`internal/agents`), so a browser never names a program. Either way it's checked to be the harness's program. |
| **An extracted memory names its turn in its metadata** | The conversation's thread and Origin place it on that turn; other memories are still placed by time ([api.md](api.md#which-memories-a-turn-produced)). |

## Search answers

The web UI's Search can write an answer from its results through the same CLI, set up on its own:
the Settings page's switch, or `meko answers use claude [--model haiku]`, `off` and `status`. Search sends the question and up to 10
of its results, taken in turn from each source, to `POST /datapacks/:id/answer`; Claude Code runs as
extraction runs it (no tools, MCP servers or saved session, in an empty folder) with a prompt to
answer from the numbered passages only, cite them as [n], and say so when they don't answer. Its
reply is held to `{"answer", "cited"}` by a JSON schema, and citations of passages it wasn't given
are dropped. Each search costs one call of the user's quota, which is why it's off until set up.

## Not built yet

Codex (`codex exec`) and Cursor (`cursor-agent -p`) adapters; extracting turns stored before the
extractor was turned on.
