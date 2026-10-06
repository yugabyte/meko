// AgentK Local's mod: a Claude Code plugin of function hooks that links a session AgentK runs back
// to the AgentK server. The server copies this folder to <AgentK home>/run/mod/agentk and launches
// each session with --plugin-dir pointing there. The mod talks to the server over the hook socket
// (AK_HOOK_SOCK) with the session's hook token (AK_HOOK_TOKEN), like the hook scripts do:
//
//   /mod/hello    once per load: the server sends this session's prompts through the mod from now on
//   /mod/poll     held by the server up to 20 s; answers the prompts AgentK has for the session,
//                 and tells it which of the prompts the mod submitted Claude still holds
//   /mod/held     the same, as soon as that changes (a prompt started its turn, or was refused)
//   /mod/ack      each prompt taken (accepted; submitted only when the server answers 204), or
//                 refused by another plugin (dropped)
//   /mod/measure  the context window's fill and the plan limits, after each turn
//   /mod/ask      Claude's AskUserQuestion questions, held by the server up to 20 s and asked again
//                 until the user answers them in AgentK Local; the terminal's dialog stays open
//                 meanwhile, and whichever answers first wins
//   /mod/ask/done the call has its answer (or ended): the server stops offering the questions
//   /mod/questions the design's questions from the design_questions tool the mod gives Claude
//
// It must never get in Claude's way: every failure is swallowed (a question is then left to the
// terminal's dialog), and while the server doesn't hear from the mod it types prompts into the
// terminal as it did before.
//
// This file is compiled by Claude Code, not by the repo's tsc: `claude-code` is the engine's own
// module, typed by the declarations Claude Code writes into .claude-plugin/types when it loads the mod.
import type { EngineInterface, Register } from 'claude-code'

/** The protocol version the server checks in /mod/hello. */
const PROTOCOL = 1
/** How long to wait after a poll failed (the server restarting, say) before the next one. */
const RETRY_MS = 3000
/** The tool the mod gives Claude for the design's questions; Claude calls it `mcp__agentk__design_questions` (`mcp__<plugin>__<name>`). */
const QUESTIONS_TOOL = 'design_questions'
const QUESTIONS_FAILED = "AgentK Local didn't take the questions. Write them to the questions file your Design prompt names instead."

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
/** The prompts submitted that Claude still holds (it runs each when it's next idle), by id. */
const held = new Set<string>()

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
    held.add(p.id)
    // Not awaited: it resolves only when the prompt's turn starts (or another plugin refused it),
    // and polling goes on meanwhile.
    void $.prompt.submit({ text: p.text, asUser: true })
      .then((r) => (r.drop !== undefined ? refused($, l, p, r.drop) : undefined))
      .catch(() => undefined)
      .finally(() => {
        held.delete(p.id)
        void post($, l, '/mod/held', { held: [...held] }).catch(() => undefined)
      })
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

/** A JSON body's fields, or {} when it isn't a JSON object. */
function json(text: string | undefined): Record<string, unknown> {
  try {
    const v: unknown = JSON.parse(text ?? '')
    return v && typeof v === 'object' ? (v as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/**
 * The user's answers in AgentK Local to Claude's questions, by question index: asks the server
 * (each ask held up to 20 s) until it has them. Never resolves when the server has none to give
 * (it said so, or the call settled meanwhile): the terminal's dialog answers then.
 */
async function answers($: EngineInterface, l: Link, id: string, questions: unknown[], call: { settled: boolean }): Promise<Record<string, string>> {
  while (!call.settled) {
    const r = await post($, l, '/mod/ask', { id, questions }).catch(() => null)
    if (call.settled) break
    if (r?.status === 200) {
      const a = json(r.text).answers
      if (a && typeof a === 'object') return a as Record<string, string>
      break
    }
    if (r?.status === 204) continue
    // 410 (the server dropped it), 400 (questions it can't show), 403: the terminal answers alone.
    if (r && r.status !== 500 && r.status !== 503) break
    await $.clock.sleep(RETRY_MS).catch(() => undefined)
  }
  return new Promise<Record<string, string>>(() => undefined)
}

/**
 * Gives Claude the design_questions tool (it takes the design's questions in the Design stage). A
 * failure leaves Claude without it, to write the questions file instead.
 */
async function registerQuestionsTool($: EngineInterface): Promise<void> {
  await $.tool.register({
    name: QUESTIONS_TOOL,
    description:
      "Gives the user the open questions of the design you're writing in AgentK Local's Design stage. AgentK Local checks them, " +
      'shows them beside the design at once, and saves them to the questions file. Pass all of them each time (the list replaces ' +
      'the last one), open and resolved, keeping each id. Returns what was saved, or what to fix in the input before calling again.',
    inputSchema: {
      type: 'object',
      properties: {
        questions: {
          type: 'array',
          items: {
            type: 'object',
            properties: {
              id: { type: 'string', description: 'Short and stable across revisions.' },
              question: { type: 'string' },
              context: { type: ['string', 'null'], description: 'Why it matters (optional).' },
              options: {
                type: 'array',
                items: { type: 'object', properties: { label: { type: 'string' }, detail: { type: ['string', 'null'] } }, required: ['label'] },
              },
              recommended: { type: ['integer', 'null'], description: 'The index of the option you recommend.' },
              status: { type: 'string', enum: ['open', 'resolved'] },
              decision: { type: ['string', 'null'], description: 'What was decided, once resolved.' },
            },
            required: ['id', 'question'],
          },
        },
      },
      required: ['questions'],
    },
  }).catch(() => undefined)
}

async function poll($: EngineInterface, l: Link): Promise<void> {
  let wait = 1
  try {
    const r = await post($, l, '/mod/poll', { held: [...held] })
    if (r.status === 200) await take($, l, (JSON.parse(r.text) as { prompts?: Prompt[] }).prompts ?? [])
    else if (r.status !== 204) wait = RETRY_MS
  } catch {
    wait = RETRY_MS
  }
  $.clock.after(wait, () => void poll($, l))
}

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    // Before next: a tool registered then is Claude's from the first turn.
    if (await linkOf($).catch(() => null)) await registerQuestionsTool($)
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

  // Claude's questions: the terminal's dialog opens as always (next), and the questions go to
  // AgentK Local too. The first answer wins; returning AgentK Local's while the dialog is open
  // closes it.
  on('tool.call', { tool: 'AskUserQuestion' }, async ($, e, next) => {
    const l = await linkOf($).catch(() => null)
    const questions = (e as unknown as { questions?: unknown }).questions
    if (!l || !Array.isArray(questions) || !questions.length) return next(e)
    const id = e.tool_use_id ?? `ask-${Date.now()}`
    const call = { settled: false }
    try {
      const won = await Promise.race([
        next(e).then((r) => ({ r })),
        answers($, l, id, questions, call).then((a) => ({ a })),
      ])
      if ('r' in won) return won.r
      const byText: Record<string, string> = {}
      for (const [i, text] of Object.entries(won.a)) {
        const q = questions[Number(i)] as { question?: unknown } | undefined
        if (typeof q?.question === 'string' && typeof text === 'string') byText[q.question] = text
      }
      return { result: { questions, answers: byText } }
    } finally {
      call.settled = true
      void post($, l, '/mod/ask/done', { id }).catch(() => undefined)
    }
  }).catch(($, e, next) => next(e))

  on('tool.call', { tool: 'mcp__agentk__design_questions' }, async ($, e) => {
    const l = await linkOf($).catch(() => null)
    if (!l) return { deny: QUESTIONS_FAILED }
    const r = await post($, l, '/mod/questions', { questions: (e as unknown as { questions?: unknown }).questions }).catch(() => null)
    const body = json(r?.text)
    if (r?.status === 200 && typeof body.text === 'string') return { result: body.text }
    return { deny: typeof body.error === 'string' ? body.error : QUESTIONS_FAILED }
  }).catch(() => ({ deny: QUESTIONS_FAILED }))
}
