// Kode's mod: a Claude Code plugin of function hooks that links a session Kode runs back
// to the Kode server. The server copies this folder to <Kode home>/run/mod/kode and launches
// each session with --plugin-dir pointing there. The mod talks to the server over the hook socket
// (KODE_HOOK_SOCK) with the session's hook token (KODE_HOOK_TOKEN), like the hook scripts do:
//
//   /mod/hello    once per load: the server sends this session's prompts through the mod from now on
//   /mod/poll     held by the server up to 20 s; answers the prompts Kode has for the session,
//                 and tells it which of the prompts the mod submitted Claude still holds
//   /mod/held     the same, as soon as that changes (a prompt started its turn, or was refused)
//   /mod/ack      each prompt taken (accepted; submitted only when the server answers 204), or
//                 refused by another plugin (dropped)
//   /mod/measure  the context window's fill and the plan limits, after each turn
//   /mod/ask      Claude's AskUserQuestion questions, held by the server up to 20 s and asked again
//                 until the user answers them in Kode; the terminal's dialog stays open
//                 meanwhile, and whichever answers first wins
//   /mod/ask/done the call has its answer (or ended): the server stops offering the questions
//   /mod/questions the design's questions from the design_questions tool the mod gives Claude
//   /mod/learn    a learn step's results, answered at once: what forking the session found (step
//                 "facts"), then the duplicate check's answer (step "decisions"). Each part of the
//                 step (`learn` on a poll answer) and the line saying what Kode saved (`toast`)
//                 come on polls
//   /mod/learn/start  /kode_learn in the terminal: start a learn step now
//   /mod/meko     Claude's Meko tool calls: `start` before each runs, `end` once it's over (failed or
//                 interrupted too), for the session's dot to show while one runs
//   /mod/turn     a turn the person interrupted (Ctrl+C, Esc), which fires no Stop hook: Kode ends
//                 the turn and sends what waited for its end
//
// A reply of just "OK" in the turn of a prompt Kode marks `hidden` (a workshop host's events) draws
// as nothing; the full transcript (ctrl+o) shows it. The prompt's own row, and every other prompt of
// Kode's, is drawn by Kode's view mod (src/mod/agentk-view): Claude Code never raises this
// plugin's `ui.render` hook for the rows of prompts it submitted itself.
//
// It must never get in Claude's way: every failure is swallowed (a question is then left to the
// terminal's dialog), and while the server doesn't hear from the mod it types prompts into the
// terminal as it did before.
//
// This file is compiled by Claude Code, not by the repo's tsc: `claude-code` is the engine's own
// module, typed by the declarations Claude Code writes into .claude-plugin/types when it loads the mod.
import type { EngineInterface, ModelForkResult, Register } from 'claude-code'

/**
 * The protocol the server checks, on /mod/hello and on every poll: 2 runs learn steps. Kode loads
 * the mod only into Claude Code that has what they use (MIN_MOD_CLAUDE in src/server/claude.ts).
 */
const PROTOCOL = 2
/** How long to wait after a poll failed (the server restarting, say) before the next one. */
const RETRY_MS = 3000
/** The tool the mod gives Claude for the design's questions; Claude calls it `mcp__kode__design_questions` (`mcp__<plugin>__<name>`). */
const QUESTIONS_TOOL = 'design_questions'
const QUESTIONS_FAILED = "Kode didn't take the questions. Write them to the questions file your Design prompt names instead."
/** The slash command that saves what the session learned to Meko. */
const LEARN_COMMAND = 'kode_learn'
/** Meko's tools, under its own MCP server's name or a plugin's (`mcp__plugin_x_meko__…`), as mekousage.ts reads them. */
const MEKO_TOOL = /^mcp__(?:[\w-]+_)?meko__/
/** The most the duplicate check may write. */
const DEDUP_MAX_TOKENS = 8000

interface Link {
  sock: string
  token: string
}

interface Prompt {
  id: string
  text: string
  /** A workshop host's Kode events: a bare "OK" in its turn draws as nothing (the view mod hides the prompt). */
  hidden?: boolean
}

/** A part of a learn step from the server: fork the session with `prompt` (extract), or run it on `model` (check). */
interface LearnJob {
  id: string
  kind: 'extract' | 'check'
  prompt: string
  model?: string
}

// Per load: a reload of the module evaluates it again, runs register() and session.start again,
// and starts over.
let link: Promise<Link | null> | null = null
let polling = false
/** The prompts submitted that Claude still holds (it runs each when it's next idle), by id. */
const held = new Set<string>()
/** A hidden prompt's turn is running: a reply of just "OK" (nothing to say) draws as nothing. */
let quietTurn = false
/** Those replies' rows, by row id, so a redraw after the turn keeps them hidden. */
const quietRows = new Set<string>()
/** What a quiet reply's row draws. */
const NOTHING = { type: 'Box', props: { display: 'none' } } as const

/** The server's socket and this session's token, or null outside a Kode session. */
function linkOf($: EngineInterface): Promise<Link | null> {
  link ??= (async () => {
    const sock = (await $.env.get('KODE_HOOK_SOCK')) ?? (await $.env.get('AK_HOOK_SOCK'))
    const token = (await $.env.get('KODE_HOOK_TOKEN')) ?? (await $.env.get('AK_HOOK_TOKEN'))
    return sock && token ? { sock, token } : null
  })()
  return link
}

function post($: EngineInterface, l: Link, path: string, body: unknown) {
  return $.http.fetch(`http://kode${path}`, {
    method: 'POST',
    socketPath: l.sock,
    headers: { 'Content-Type': 'application/json', 'X-AK-Token': l.token },
    body: JSON.stringify(body),
  })
}

/**
 * Submits Kode's prompts as the person's own (the model reads them bare, not as a plugin's
 * message). Each is acked first and submitted only when the server answers 204: any other answer,
 * or no answer, means Kode gave up on it (and may send it again), so running it would run it
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
      .then((r) => {
        if (r.drop !== undefined) return refused($, l, p, r.drop)
        if (p.hidden) quietTurn = true
      })
      .catch(() => undefined)
      .finally(() => {
        held.delete(p.id)
        void post($, l, '/mod/held', { held: [...held] }).catch(() => undefined)
      })
  }
}

/**
 * Another plugin's `prompt.submit` hook refused Kode's prompt. The prompt box gets its text when
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
 * The user's answers in Kode to Claude's questions, by question index: asks the server
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
      "Gives the user the open questions and key design decisions of the design you're writing in Kode's Design stage. Kode " +
      'checks them, shows them beside the design at once, and saves them to the questions file. Pass all of them each time (the list ' +
      'replaces the last one): open and resolved questions, and the decisions you made yourself (kind "decision"), keeping each id. ' +
      'Returns what was saved, or what to fix in the input before calling again.',
    inputSchema: {
      type: 'object',
      properties: {
        questions: {
          type: 'array',
          items: {
            type: 'object',
            properties: {
              id: { type: 'string', description: 'Short and stable across revisions.' },
              kind: { type: 'string', enum: ['question', 'decision'], description: 'decision: one you made yourself, always resolved. Default question.' },
              question: { type: 'string', description: 'The question, or for a decision, the decision point.' },
              context: { type: ['string', 'null'], description: 'Why it matters, or why you decided so (optional).' },
              options: {
                type: 'array',
                description: 'The real alternatives, when there are any; "recommended" is an index into this list.',
                items: { type: 'object', properties: { label: { type: 'string' }, detail: { type: ['string', 'null'] } }, required: ['label'] },
              },
              recommended: { type: ['integer', 'null'], description: 'The index of the option you recommend, or for a decision, picked.' },
              status: { type: 'string', enum: ['open', 'resolved'], description: 'open until the user answers it or it is decided; a decision is always resolved.' },
              decision: { type: ['string', 'null'], description: 'What was decided, once resolved.' },
              decidedBy: { type: ['string', 'null'], enum: ['user', 'kode', null], description: 'Who decided it, once resolved.' },
              meko: {
                type: 'array',
                description: 'The Meko items it rests on, by the id Meko gave you. Never invent one.',
                items: {
                  type: 'object',
                  properties: {
                    kind: { type: 'string', enum: ['memory', 'knowledge'] },
                    id: { type: 'string' },
                    datapackId: { type: ['string', 'null'] },
                    excerpt: { type: ['string', 'null'], description: 'The words you relied on.' },
                  },
                  required: ['kind', 'id'],
                },
              },
            },
            required: ['id', 'question'],
          },
        },
      },
      required: ['questions'],
    },
  }).catch(() => undefined)
}

/** What a model call came to, as /mod/learn takes it: the reply's text, or why there is none, and its usage. */
function modelReply(r: ModelForkResult | null): Record<string, unknown> {
  if (!r) return { reason: 'the model call failed' }
  if (r.isAnswered) return { text: r.text, usage: r.usage }
  return { reason: r.reason, usage: 'usage' in r ? r.usage : null }
}

/**
 * A part of a learn step. `extract` asks the session's own context (a fork: no tools, nothing added
 * to the transcript, the prefix served from the prompt cache) what it learned; `check` runs the
 * duplicate check Kode built from what Meko already has on a small model. Either way the reply
 * goes to /mod/learn, which answers at once. Started from a timer by poll, so it outlives the
 * poll's dispatch; each call is guarded on its own, so a failure reaches Kode as a reason at once.
 */
async function learn($: EngineInterface, l: Link, job: LearnJob): Promise<void> {
  let reply: ModelForkResult | null = null
  let model: string | null = job.model ?? null
  if (job.kind === 'check') {
    try {
      reply = await $.model.complete({ model: job.model ?? 'haiku', prompt: job.prompt, maxTokens: DEDUP_MAX_TOKENS })
    } catch {
      // reported as the call failing
    }
    await post($, l, '/mod/learn', { id: job.id, step: 'decisions', model, ...modelReply(reply) }).catch(() => undefined)
    return
  }
  try {
    model = await $.session.model()
  } catch {
    // the model only labels the usage
  }
  try {
    reply = await $.model.fork({ prompt: job.prompt })
  } catch {
    // reported as the call failing
  }
  await post($, l, '/mod/learn', { id: job.id, step: 'facts', model, ...modelReply(reply) }).catch(() => undefined)
}

/** Shows a line from Kode (what a learn step saved). */
function toast($: EngineInterface, text: string): void {
  try {
    $.ui.toast(text)
  } catch {
    // the timeline has it too
  }
}

/** Gives the terminal /kode_learn. A Claude Code without slash commands for plugins goes without it. */
async function registerLearnCommand($: EngineInterface): Promise<void> {
  try {
    await $.command.register({ name: LEARN_COMMAND, description: "Save what this session learned to the project's Meko datapack (Kode)" })
  } catch {
    // Save learnings in Kode does the same
  }
}

async function poll($: EngineInterface, l: Link): Promise<void> {
  let wait = 1
  try {
    // The protocol too: a server restarted since hello hears it here.
    const r = await post($, l, '/mod/poll', { held: [...held], protocol: PROTOCOL })
    if (r.status === 200) {
      const answer = JSON.parse(r.text) as { prompts?: Prompt[]; learn?: LearnJob; toast?: unknown }
      const job = answer.learn
      if (typeof job?.id === 'string' && typeof job.prompt === 'string') $.clock.after(1, () => void learn($, l, job).catch(() => undefined))
      if (typeof answer.toast === 'string') toast($, answer.toast)
      await take($, l, answer.prompts ?? [])
    } else if (r.status !== 204) wait = RETRY_MS
  } catch {
    wait = RETRY_MS
  }
  $.clock.after(wait, () => void poll($, l))
}

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    // Before next: a tool registered then is Claude's from the first turn.
    if (await linkOf($).catch(() => null)) {
      await registerQuestionsTool($)
      await registerLearnCommand($)
    }
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

  // A turn the person interrupted fires no Stop hook: Kode hears of it here, before the engine
  // goes idle and runs a prompt it holds, so that prompt's turn never counts as the one that ended.
  // A subagent's turn, and every turn Stop covers, goes straight on.
  on('turn.complete', async ($, e, next) => {
    if (!e.agentId) quietTurn = false
    if (!e.isAborted || e.agentId) return next(e)
    const l = await linkOf($).catch(() => null)
    if (l) await post($, l, '/mod/turn', { turnId: e.turnId, reason: 'aborted' }).catch(() => undefined)
    return next(e)
  }).catch(($, e, next) => next(e))

  // Claude's questions: the terminal's dialog opens as always (next), and the questions go to
  // Kode too. The first answer wins; returning Kode's while the dialog is open
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

  // Claude's Meko calls: Kode hears of each before it runs (awaited, so `start` always comes
  // before `end`) and once it's over, whatever the outcome. The call itself is left alone. The
  // tool is checked here rather than with a RegExp matcher, which the oldest Claude Code the mod
  // runs in (MIN_MOD_CLAUDE) may not take; every other tool goes straight on. The `.catch` replays
  // what `next(e)` settled to, so a failed call never runs twice.
  on('tool.call', async ($, e, next) => {
    if (!MEKO_TOOL.test(e.tool) || !e.tool_use_id) return next(e)
    const l = await linkOf($).catch(() => null)
    if (!l) return next(e)
    const id = e.tool_use_id
    await post($, l, '/mod/meko', { id, state: 'start' }).catch(() => null)
    try {
      return await next(e)
    } finally {
      void post($, l, '/mod/meko', { id, state: 'end' }).catch(() => undefined)
    }
  }).catch(($, e, next) => next(e))

  // /kode_learn: Kode starts a learn step (the next poll brings it), or says why it can't.
  on('command.run', { command: LEARN_COMMAND }, async ($) => {
    const l = await linkOf($).catch(() => null)
    if (!l) return { text: 'This session is not run by Kode.' }
    const r = await post($, l, '/mod/learn/start', {}).catch(() => null)
    const text = json(r?.text).text
    return { text: typeof text === 'string' ? text : "Kode didn't answer. Use Save learnings to Meko in the session's ⋯ menu in Kode instead." }
  }).catch(() => ({ text: "Kode didn't answer. Use Save learnings to Meko in the session's ⋯ menu in Kode instead." }))

  // A quiet "OK" in a hidden prompt's turn draws as nothing; the full transcript (ctrl+o) shows it.
  // Registered in a try: a Claude Code without the hook still runs the rest.
  try {
    on('ui.render', { component: 'AssistantMessage' }, ($, e, next) => {
      const p = e.props as { text?: unknown }
      if (quietTurn && typeof p.text === 'string' && p.text.trim() === 'OK') quietRows.add(e.requestId)
      return quietRows.has(e.requestId) ? NOTHING : next(e)
    }).catch(($, e, next) => next(e))
  } catch {
    // The rows draw as Claude Code draws them
  }

  on('tool.call', { tool: 'mcp__kode__design_questions' }, async ($, e) => {
    const l = await linkOf($).catch(() => null)
    if (!l) return { deny: QUESTIONS_FAILED }
    const r = await post($, l, '/mod/questions', { questions: (e as unknown as { questions?: unknown }).questions }).catch(() => null)
    const body = json(r?.text)
    if (r?.status === 200 && typeof body.text === 'string') return { result: body.text }
    return { deny: typeof body.error === 'string' ? body.error : QUESTIONS_FAILED }
  }).catch(() => ({ deny: QUESTIONS_FAILED }))
}
