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
//   /mod/learn    a learn step's results, answered at once: what forking the session found (step
//                 "facts"), then the duplicate check's answer (step "decisions"). Each part of the
//                 step (`learn` on a poll answer) and the line saying what AgentK saved (`toast`)
//                 come on polls
//   /mod/learn/start  /agentk_learn in the terminal: start a learn step now
//   /mod/meko     Claude's Meko tool calls: `start` before each runs, `end` once it's over (failed or
//                 interrupted too), for the session's dot to show while one runs. While the project
//                 keeps a Meko call log, `start` is answered `{ log: true }` and `end` carries the call:
//                 the tool, what Claude sent, the reply as Claude read it, how it went and how long it took
//   /mod/turn     a turn the person interrupted (Ctrl+C, Esc), which fires no Stop hook: AgentK ends
//                 the turn and sends what waited for its end
//
// It must never get in Claude's way: every failure is swallowed (a question is then left to the
// terminal's dialog), and while the server doesn't hear from the mod it types prompts into the
// terminal as it did before.
//
// This file is compiled by Claude Code, not by the repo's tsc: `claude-code` is the engine's own
// module, typed by the declarations Claude Code writes into .claude-plugin/types when it loads the mod.
import type { EngineInterface, ModelForkResult, Register, ToolCallInput, ToolCallResult } from 'claude-code'

/**
 * The protocol the server checks, on /mod/hello and on every poll: 2 runs learn steps. AgentK loads
 * the mod only into Claude Code that has what they use (MIN_MOD_CLAUDE in src/server/claude.ts).
 */
const PROTOCOL = 2
/** How long to wait after a poll failed (the server restarting, say) before the next one. */
const RETRY_MS = 3000
/** The tool the mod gives Claude for the design's questions; Claude calls it `mcp__agentk__design_questions` (`mcp__<plugin>__<name>`). */
const QUESTIONS_TOOL = 'design_questions'
const QUESTIONS_FAILED = "AgentK Local didn't take the questions. Write them to the questions file your Design prompt names instead."
/** The slash command that saves what the session learned to Meko. */
const LEARN_COMMAND = 'agentk_learn'
/** Meko's tools, under its own MCP server's name or a plugin's (`mcp__plugin_x_meko__…`), as mekousage.ts reads them. */
const MEKO_TOOL = /^mcp__(?:[\w-]+_)?meko__/
/** Keys of a `tool.call` input that aren't the tool's own arguments. */
const RESERVED_KEYS = new Set(['tool', 'tool_use_id', 'consent', 'agentId', 'requestMeta'])
/** The most a Meko call sent to the log may take: the hook socket drops a body over 4 MB. */
const MAX_LOGGED_CHARS = 3.5 * 1024 * 1024
/** The most of a call's error that's sent: a failed reply's text, a refusal or what was thrown. */
const ERROR_CHARS = 300
/** The most the duplicate check may write. */
const DEDUP_MAX_TOKENS = 8000

interface Link {
  sock: string
  token: string
}

interface Prompt {
  id: string
  text: string
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

/** How a tool call ended: what `next(e)` resolved to, or what it threw. */
type Settled = { r: ToolCallResult } | { thrown: unknown }

/**
 * One of Claude's Meko calls, for the session's Meko call log: what it sent (its input without the
 * engine's keys), the reply as Claude read it, and how it went. A call too large for the hook
 * socket goes without its reply, then without its arguments too, so it's still logged.
 */
function mekoCall(e: ToolCallInput, s: Settled, ms: number) {
  const args: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(e)) if (!RESERVED_KEYS.has(k)) args[k] = v
  let ok = false
  let text: string | null = null
  let error: string | null = null
  if (!('r' in s)) {
    error = s.thrown instanceof Error && s.thrown.message ? s.thrown.message : 'Interrupted'
  } else if (s.r.deny !== undefined) {
    error = s.r.deny
  } else {
    const { result } = s.r
    text = typeof s.r.text === 'string' ? s.r.text : typeof result === 'string' ? result : result === undefined ? null : JSON.stringify(result)
    ok = !s.r.isError
    if (!ok) error = text || 'Meko reported an error'
  }
  if (error !== null) error = error.slice(0, ERROR_CHARS)
  const call = { tool: e.tool, args: args as unknown, ms, ok, text, error, subagent: !!e.agentId }
  const size = JSON.stringify(call).length
  if (size <= MAX_LOGGED_CHARS) return call
  const tooLarge = `The call was too large to log (${size} characters).`
  const lean = { ...call, text: null, error: error ? `${error} ${tooLarge}` : tooLarge }
  return JSON.stringify(lean).length <= MAX_LOGGED_CHARS ? lean : { ...lean, args: null }
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
      "Gives the user the open questions and key design decisions of the design you're writing in AgentK Local's Design stage. AgentK Local " +
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
              decidedBy: { type: ['string', 'null'], enum: ['user', 'agentk', null], description: 'Who decided it, once resolved.' },
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
 * duplicate check AgentK built from what Meko already has on a small model. Either way the reply
 * goes to /mod/learn, which answers at once. Started from a timer by poll, so it outlives the
 * poll's dispatch; each call is guarded on its own, so a failure reaches AgentK as a reason at once.
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

/** Shows a line from AgentK (what a learn step saved). */
function toast($: EngineInterface, text: string): void {
  try {
    $.ui.toast(text)
  } catch {
    // the timeline has it too
  }
}

/** Gives the terminal /agentk_learn. A Claude Code without slash commands for plugins goes without it. */
async function registerLearnCommand($: EngineInterface): Promise<void> {
  try {
    await $.command.register({ name: LEARN_COMMAND, description: "Save what this session learned to the project's Meko datapack (AgentK)" })
  } catch {
    // Save learnings in AgentK Local does the same
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

  // A turn the person interrupted fires no Stop hook: AgentK hears of it here, before the engine
  // goes idle and runs a prompt it holds, so that prompt's turn never counts as the one that ended.
  // A subagent's turn, and every turn Stop covers, goes straight on.
  on('turn.complete', async ($, e, next) => {
    if (!e.isAborted || e.agentId) return next(e)
    const l = await linkOf($).catch(() => null)
    if (l) await post($, l, '/mod/turn', { turnId: e.turnId, reason: 'aborted' }).catch(() => undefined)
    return next(e)
  }).catch(($, e, next) => next(e))

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

  // Claude's Meko calls: AgentK hears of each before it runs (awaited, so `start` always comes
  // before `end`) and once it's over, whatever the outcome; while the project keeps a Meko call log
  // (`start` answered `{ log: true }`), the end carries the call. The call itself is left alone. The
  // tool is checked here rather than with a RegExp matcher, which the oldest Claude Code the mod
  // runs in (MIN_MOD_CLAUDE) may not take; every other tool goes straight on. The `.catch` replays
  // what `next(e)` settled to, so a failed call never runs twice.
  on('tool.call', async ($, e, next) => {
    if (!MEKO_TOOL.test(e.tool) || !e.tool_use_id) return next(e)
    const l = await linkOf($).catch(() => null)
    if (!l) return next(e)
    const id = e.tool_use_id
    const started = await post($, l, '/mod/meko', { id, state: 'start' }).catch(() => null)
    const log = started?.status === 200 && json(started.text).log === true
    const at = Date.now()
    let settled: Settled = { thrown: undefined }
    try {
      const r = await next(e)
      settled = { r }
      return r
    } catch (err) {
      settled = { thrown: err }
      throw err
    } finally {
      const end = log ? { id, state: 'end', ...mekoCall(e, settled, Date.now() - at) } : { id, state: 'end' }
      void post($, l, '/mod/meko', end).catch(() => undefined)
    }
  }).catch(($, e, next) => next(e))

  // /agentk_learn: AgentK starts a learn step (the next poll brings it), or says why it can't.
  on('command.run', { command: LEARN_COMMAND }, async ($) => {
    const l = await linkOf($).catch(() => null)
    if (!l) return { text: 'This session is not run by AgentK Local.' }
    const r = await post($, l, '/mod/learn/start', {}).catch(() => null)
    const text = json(r?.text).text
    return { text: typeof text === 'string' ? text : "AgentK Local didn't answer. Use Save learnings in the session's header in AgentK Local (⋯ on a phone) instead." }
  }).catch(() => ({ text: "AgentK Local didn't answer. Use Save learnings in the session's header in AgentK Local (⋯ on a phone) instead." }))

  on('tool.call', { tool: 'mcp__agentk__design_questions' }, async ($, e) => {
    const l = await linkOf($).catch(() => null)
    if (!l) return { deny: QUESTIONS_FAILED }
    const r = await post($, l, '/mod/questions', { questions: (e as unknown as { questions?: unknown }).questions }).catch(() => null)
    const body = json(r?.text)
    if (r?.status === 200 && typeof body.text === 'string') return { result: body.text }
    return { deny: typeof body.error === 'string' ? body.error : QUESTIONS_FAILED }
  }).catch(() => ({ deny: QUESTIONS_FAILED }))
}
