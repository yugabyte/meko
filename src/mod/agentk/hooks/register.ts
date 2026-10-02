// AgentK Local's mod: a Claude Code plugin of function hooks that links a session AgentK runs back
// to the AgentK server. The server copies this folder to <AgentK home>/run/mod/agentk and launches
// each session with --plugin-dir pointing there. The mod talks to the server over the hook socket
// (AK_HOOK_SOCK) with the session's hook token (AK_HOOK_TOKEN), like the hook scripts do:
//
//   /mod/hello    once per load: the server sends this session's prompts through the mod from now on
//   /mod/poll     held by the server up to 20 s; answers the prompts AgentK has for the session
//   /mod/ack      each prompt taken (accepted; submitted only when the server answers 204), or
//                 refused by another plugin (dropped)
//   /mod/measure  the context window's fill and the plan limits, after each turn
//
// It must never get in Claude's way: every failure is swallowed, and while the server doesn't hear
// from the mod it types prompts into the terminal as it did before.
//
// This file is compiled by Claude Code, not by the repo's tsc: `claude-code` is the engine's own
// module, typed by the declarations Claude Code writes into .claude-plugin/types when it loads the mod.
import type { EngineInterface, Register } from 'claude-code'

/** The protocol version the server checks in /mod/hello. */
const PROTOCOL = 1
/** How long to wait after a poll failed (the server restarting, say) before the next one. */
const RETRY_MS = 3000

interface Link {
  sock: string
  token: string
}

interface Prompt {
  id: string
  text: string
}

// Per load: a reload of the module evaluates it again, runs register() and session.start again,
// and starts over.
let link: Promise<Link | null> | null = null
let polling = false

/** The server's socket and this session's token, or null outside an AgentK session. */
function linkOf($: EngineInterface): Promise<Link | null> {
  link ??= (async () => {
    const sock = await $.env.get('AK_HOOK_SOCK')
    const token = await $.env.get('AK_HOOK_TOKEN')
    return sock && token ? { sock, token } : null
  })()
  return link
}

function post($: EngineInterface, l: Link, path: string, body: unknown) {
  return $.http.fetch(`http://agentk${path}`, {
    method: 'POST',
    socketPath: l.sock,
    headers: { 'Content-Type': 'application/json', 'X-AK-Token': l.token },
    body: JSON.stringify(body),
  })
}

/**
 * Submits AgentK's prompts as the person's own (the model reads them bare, not as a plugin's
 * message). Each is acked first and submitted only when the server answers 204: any other answer,
 * or no answer, means AgentK gave up on it (and may send it again), so running it would run it
 * twice. The engine holds a submitted prompt until the session is idle, so it never lands in a
 * dialog or a running turn.
 */
async function take($: EngineInterface, l: Link, prompts: Prompt[]) {
  for (const p of prompts) {
    if (typeof p?.id !== 'string' || typeof p.text !== 'string') continue
    const ack = await post($, l, '/mod/ack', { id: p.id, state: 'accepted' }).catch(() => null)
    if (ack?.status !== 204) continue
    // Not awaited: it resolves only when the prompt's turn starts, and polling goes on meanwhile.
    void $.prompt.submit({ text: p.text, asUser: true }).then(
      (r) => (r.drop !== undefined ? refused($, l, p, r.drop) : undefined),
    ).catch(() => undefined)
  }
}

/**
 * Another plugin's `prompt.submit` hook refused AgentK's prompt. The prompt box gets its text when
 * it's empty, for the person to send, and the server is told, to ask them to step in.
 */
async function refused($: EngineInterface, l: Link, p: Prompt, reason: string) {
  const box = await $.prompt.read().catch(() => null)
  const filled = box?.text === '' && (await $.prompt.fill({ text: p.text }).catch(() => null))?.isFilled === true
  await post($, l, '/mod/ack', { id: p.id, state: 'dropped', reason, filled })
}

async function poll($: EngineInterface, l: Link): Promise<void> {
  let wait = 1
  try {
    const r = await post($, l, '/mod/poll', {})
    if (r.status === 200) await take($, l, (JSON.parse(r.text) as { prompts?: Prompt[] }).prompts ?? [])
    else if (r.status !== 204) wait = RETRY_MS
  } catch {
    wait = RETRY_MS
  }
  $.clock.after(wait, () => void poll($, l))
}

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    const started = await next(e)
    const l = await linkOf($).catch(() => null)
    if (!l || polling) return started
    polling = true
    await post($, l, '/mod/hello', { protocol: PROTOCOL }).catch(() => undefined)
    // From a timer, so the first poll isn't held inside session.start's dispatch.
    $.clock.after(1, () => void poll($, l))
    return started
  })

  on('session.measure', async ($, e, next) => {
    const l = await linkOf($).catch(() => null)
    if (l) await post($, l, '/mod/measure', { context: e.context, rateLimits: e.rateLimits }).catch(() => undefined)
    return next(e)
  })
}
