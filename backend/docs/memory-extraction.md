# Meko backend: memory extraction

Meko pulls memories out of conversation turns through the user's own AI tool's CLI, run headless:
Claude Code today (`claude -p`), Codex and Cursor later. It's off until `meko memory-extractor use
claude` turns it on.

## How it works

1. **Queue.** While the extractor is on, `conversation_add_message` queues each new turn
   (`extraction_queue`, in the transaction that stores it), unless its datapack opted out.
2. **Run.** Every interval (5 minutes by default, at least 1), and at once on `meko memory-extractor
   run` or a settings change, a run takes each conversation with waiting turns, up to 30 of them.
3. **Prepare.** Turns with nothing to remember (under 12 characters: "ok", "thanks") leave the queue
   without a call. The rest go in one call per conversation: today's date, the 20 memories of the
   datapack nearest the turns (the embedder finds them), the 4 turns before as context, and the new
   turns numbered, the user's text cut to its first and last characters past 4,000, each reply to 300.
4. **Extract.** The CLI returns `{"memory": [{"text", "turn"}]}`, its JSON schema enforced.
5. **Save.** A fact within 0.9 cosine of an existing memory is dropped; the rest are added through
   the memory engine as the turn's conversation's agent, with `meko_source: "extract"` and
   `meko_message_id` (the turn) in their metadata, and the turns leave the queue.

A failed call (not signed in, a timeout, bad JSON) leaves its turns queued with `attempts` and
`last_error`, ends the run, and is shown by `meko memory-extractor status`, `meko status`'s web
counterpart (the Status page) and `GET /settings/memory-extractor`; the next run tries again.

## Decisions

| Decision | Why |
|---|---|
| **The user's own CLI, not a bundled or hosted model** | Meko local runs beside a strong LLM already: the user's agent. No model to download, no key to keep, and the quota is the user's choice. A small local model extracts poorly from coding conversations. |
| **Meko Cloud's rules: the user's messages only, add only** | Cloud passes only the turn's input to mem0 2.0's additive extraction (meko-mcp-server `_run_memory_extraction`), so the agent's general-knowledge answers don't become memories; replies are context. Adding only, with the nearest existing memories shown, matches Cloud and needs no update or delete step. |
| **A short prompt, with profiles and the datapack's own instructions** (`extractprompt.go`) | Cloud's prompt is about 600 lines; every call pays for it. The base keeps what decides the outcome. A datapack picks `general` or `coding` (rules and examples for coding conversations: keep conventions, stack, decisions, recurring fixes; skip transient code, pasted files and logs) and may add instructions (`meko memory-extractor datapack <name> --profile coding --instructions "..."`). |
| **A fixed interval, one call per conversation** | Predictable, and a call covers every turn waiting, so its fixed cost is shared. |
| **Claude Code runs with no tools, MCP servers, slash commands or saved session, and only the local settings of an empty folder** | Turns are untrusted text. The empty folder's settings keep the user's hooks (Meko's own capture among them) from running inside the call; `--bare` would too, but skips the saved login. `--system-prompt` replaces Claude Code's own (about 4k tokens). |
| **Setting it up is the CLI's (admin token)** | It points the server at a program to run: `PUT /settings/memory-extractor` takes the absolute path `meko memory-extractor use` finds on the user's PATH (the login service's own may not have it), checked to be the harness's program, and refuses the client token and the browser. |
| **An extracted memory names its turn in its metadata** | The conversation's thread and Origin place it on that turn; other memories are still placed by time ([api.md](api.md#which-memories-a-turn-produced)). |

## Not built yet

Codex (`codex exec`) and Cursor (`cursor-agent -p`) adapters; extracting turns stored before the
extractor was turned on.
