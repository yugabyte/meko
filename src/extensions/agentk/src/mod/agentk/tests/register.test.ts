// Run with `claude plugin test src/mod/agentk` (the repo's vitest suite runs it when Claude Code
// is installed: test/unit/mod.test.ts).
import { expect, mock, test } from 'claude-code/testing'
import type { HttpResponse, On, PromptSubmitInput } from 'claude-code'

const ENV = { AK_HOOK_SOCK: '/ak/hook.sock', AK_HOOK_TOKEN: 'tok' }

interface Call {
  path: string
  body: unknown
  socketPath?: string
  token?: string
}

/** What the test's http.fetch hook answers the mod: the response, as the engine's bottom would. */
const answer = (status: number, body?: unknown): { value: HttpResponse } => ({
  value: { status, ok: status < 300, headers: {}, text: body === undefined ? '' : JSON.stringify(body) },
})

/**
 * The AgentK server beneath the mod: records each request, answers polls from `prompts`, and an
 * `accepted` ack with `ackStatus` (204: submit it).
 */
function server(on: On, prompts: { id: string; text: string }[][] = [], ackStatus = 204) {
  const calls: Call[] = []
  on('http.fetch', (_$, e) => {
    const path = new URL(e.url).pathname
    calls.push({
      path,
      body: e.init?.body ? JSON.parse(e.init.body) : undefined,
      socketPath: e.init?.socketPath,
      token: e.init?.headers?.['X-AK-Token'],
    })
    if (path === '/mod/poll') {
      const next = prompts.shift()
      return next ? answer(200, { prompts: next }) : answer(204)
    }
    if (path === '/mod/ack' && (calls.at(-1)?.body as { state?: string } | undefined)?.state === 'accepted') return answer(ackStatus)
    return answer(204)
  })
  return calls
}

/**
 * The engine's own end of session.start, session.measure, prompt.submit and the prompt box (`box`
 * its draft; what the mod fills in is kept in `filled`).
 */
function engine(on: On, drop?: string, box = '') {
  const submitted: PromptSubmitInput[] = []
  const filled: string[] = []
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('session.measure', (_$, e) => ({ changed: e.changed }))
  on('prompt.read', () => ({ value: { text: box, cursor: box.length } }))
  on('prompt.fill', (_$, e) => {
    filled.push(e.text)
    return { isFilled: true }
  })
  on('prompt.submit', (_$, e) => {
    submitted.push(e)
    return drop ? { drop } : { text: e.text }
  })
  return Object.assign(submitted, { filled })
}

const START = { cwd: '/work', surface: null, isInteractive: true } as const

test('says hello, then submits each prompt AgentK has as the person own, acked first', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  const calls = server(on, [[{ id: 'p1', text: 'Implement the design.' }]])
  const submitted = engine(on)
  await $.session.start(START)
  await clock.advance(5)
  expect(calls[0]).toEqual({ path: '/mod/hello', body: { protocol: 2 }, socketPath: '/ak/hook.sock', token: 'tok' })
  expect(calls.map((c) => c.path).slice(0, 3)).toEqual(['/mod/hello', '/mod/poll', '/mod/ack'])
  expect(calls[2]?.body).toEqual({ id: 'p1', state: 'accepted' })
  expect(submitted.map((s) => s.text)).toEqual(['Implement the design.'])
  expect(submitted[0]?.origin).toEqual(expect.objectContaining({ kind: 'plugin', asUser: true }))
  // And it keeps polling.
  expect(calls.filter((c) => c.path === '/mod/poll').length).toBeGreaterThan(1)
})

test('submits nothing the server gave up on (a late ack), or when the ack fails', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  server(on, [[{ id: 'p1', text: 'Too late.' }]], 410)
  const submitted = engine(on)
  await $.session.start(START)
  await clock.advance(5)
  expect(submitted).toEqual([])
})

test('submits nothing when its ack never reaches the server', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  let polls = 0
  on('http.fetch', (_$, e) => {
    const path = new URL(e.url).pathname
    if (path === '/mod/poll' && polls++ === 0) return answer(200, { prompts: [{ id: 'p1', text: 'Lost ack.' }] })
    if (path === '/mod/ack') throw new Error('connect ENOENT')
    return answer(204)
  })
  const submitted = engine(on)
  await $.session.start(START)
  await clock.advance(5)
  expect(submitted).toEqual([])
})

test('reports a prompt another plugin refused, with its text put in an empty prompt box', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  const calls = server(on, [[{ id: 'p1', text: 'Open the PR.' }]])
  const submitted = engine(on, 'not now')
  await $.session.start(START)
  await clock.advance(5)
  expect(calls.find((c) => (c.body as { state?: string } | undefined)?.state === 'dropped')?.body)
    .toEqual({ id: 'p1', state: 'dropped', reason: 'not now', filled: true })
  expect(submitted.filled).toEqual(['Open the PR.'])
})

test("leaves the person's draft alone when another plugin refuses a prompt", async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  const calls = server(on, [[{ id: 'p1', text: 'Open the PR.' }]])
  const submitted = engine(on, 'not now', 'half-typed')
  await $.session.start(START)
  await clock.advance(5)
  expect(calls.find((c) => (c.body as { state?: string } | undefined)?.state === 'dropped')?.body)
    .toEqual({ id: 'p1', state: 'dropped', reason: 'not now', filled: false })
  expect(submitted.filled).toEqual([])
})

test('waits before polling again when the server is away', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  let polls = 0
  on('http.fetch', (_$, e) => {
    if (new URL(e.url).pathname === '/mod/poll') polls++
    throw new Error('connect ENOENT')
  })
  engine(on)
  await $.session.start(START)
  await clock.advance(5)
  expect(polls).toBe(1)
  await clock.advance(3000)
  expect(polls).toBe(2)
})

test('passes the context fill and plan limits on after a turn', async ($, on) => {
  mock.env(on, ENV)
  const calls = server(on)
  engine(on)
  const context = { tokens: 32000, window: 200000, percent: 16 }
  const rateLimits = [{ kind: 'five_hour', percentUsed: 2, resetsAt: '2026-10-02T07:30:00.000Z' }]
  await $.session.measure({ context, rateLimits, changed: ['context'] })
  expect(calls).toEqual([{ path: '/mod/measure', body: { context, rateLimits }, socketPath: '/ak/hook.sock', token: 'tok' }])
})

test('tells AgentK of a turn the person interrupted, and of no other turn', async ($, on) => {
  mock.env(on, ENV)
  const calls = server(on)
  on('turn.complete', () => ({ text: '' }))
  const turn = { answer: '', durationMs: 900, turnId: 't1' }
  await $.turn.complete({ ...turn, reason: 'aborted', isAborted: true })
  await $.turn.complete({ ...turn, turnId: 't2', reason: 'answer', isAborted: false })
  await $.turn.complete({ ...turn, turnId: 't3', reason: 'aborted', isAborted: true, agentId: 'a1' })
  expect(calls).toEqual([{ path: '/mod/turn', body: { turnId: 't1', reason: 'aborted' }, socketPath: '/ak/hook.sock', token: 'tok' }])
})

test("draws a bare OK in a hidden prompt's turn as nothing, but in full with ctrl+o", async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  const events = 'AgentK events:\n- Review the score fix: First look done'
  server(on, [[{ id: 'p1', text: events, hidden: true }]])
  engine(on)
  on('ui.render', () => ({ type: 'Text', children: ['drawn'] }))
  on('turn.complete', () => ({ text: '' }))
  const drawn = expect.objectContaining({ type: 'Text' })
  await $.session.start(START)
  await clock.advance(5)
  const origin = { kind: 'plugin', name: 'agentk', asUser: true } as const
  const reply = (requestId: string, text: string) =>
    $.ui.render({ surface: 'terminal', component: 'AssistantMessage', requestId, props: { text, isFirstOfReply: true } })
  const hidden = expect.objectContaining({ type: 'Box', props: { display: 'none' } })
  // The prompt's own row is the view mod's (src/mod/agentk-view): this mod leaves it alone.
  expect(await $.ui.render({ surface: 'terminal', component: 'UserMessage', requestId: 'u1', props: { text: events, origin, isExpanded: false } })).toEqual(drawn)
  expect(await reply('a1', 'OK')).toEqual(hidden)
  expect(await reply('a2', 'The first look missed the trap.')).toEqual(drawn)
  // After the turn, an OK is the model's own, and the hidden one stays hidden.
  await $.turn.complete({ answer: 'OK', durationMs: 900, turnId: 't1', reason: 'answer', isAborted: false })
  expect(await reply('a3', 'OK')).toEqual(drawn)
  expect(await reply('a1', 'OK')).toEqual(hidden)
})

test('does nothing outside an AgentK session', async ($, on) => {
  mock.env(on, {})
  const clock = mock.clock(on)
  const calls = server(on)
  engine(on)
  on('turn.complete', () => ({ text: '' }))
  await $.session.start(START)
  await $.session.measure({ context: { window: 200000 }, rateLimits: [], changed: ['context'] })
  await $.turn.complete({ answer: '', durationMs: 900, turnId: 't1', reason: 'aborted', isAborted: true })
  await clock.advance(5000)
  expect(calls).toEqual([])
})

const QUESTIONS = [{ question: 'Which color?', header: 'Color', options: [{ label: 'Red', description: 'Warm' }, { label: 'Blue', description: 'Cool' }], multiSelect: false }]

/** The AgentK server for asks and the questions tool: `reply` answers each request (pending: held). */
function routes(on: On, reply: (path: string, body: Record<string, unknown>) => { value: HttpResponse } | Promise<{ value: HttpResponse }>) {
  const calls: { path: string; body: Record<string, unknown> }[] = []
  on('http.fetch', (_$, e) => {
    const path = new URL(e.url).pathname
    const body = e.init?.body ? JSON.parse(e.init.body) : {}
    calls.push({ path, body })
    return reply(path, body)
  })
  return calls
}

/**
 * Claude's own AskUserQuestion dialog in the terminal, beneath the mod: `opened` once it shows,
 * then answered by `answer`, or closed.
 */
function dialog(on: On) {
  let open = () => undefined as void
  const d = { closed: false, opened: new Promise<void>((r) => (open = r)), answer: (_r: unknown) => undefined as void }
  on('tool.call', { tool: 'AskUserQuestion' }, (_$, _e, next) => new Promise((resolve) => {
    d.answer = (r) => resolve(r as never)
    open()
    next.signal.addEventListener('abort', () => {
      d.closed = true
      resolve({ deny: 'closed' })
    })
  }))
  return d
}

const held = () => new Promise<{ value: HttpResponse }>(() => undefined)

test("answers Claude's question with the user's answer from AgentK, closing the terminal's dialog", async ($, on) => {
  mock.env(on, ENV)
  const calls = routes(on, (path) => (path === '/mod/ask' ? answer(200, { answers: { 0: 'Blue' } }) : answer(204)))
  const d = dialog(on)
  const r = await $.tool.call({ tool: 'AskUserQuestion', tool_use_id: 'tu1', questions: QUESTIONS })
  expect(r.result).toEqual({ questions: QUESTIONS, answers: { 'Which color?': 'Blue' } })
  expect(d.closed).toBe(true)
  expect(calls[0]).toEqual({ path: '/mod/ask', body: { id: 'tu1', questions: QUESTIONS } })
  expect(calls.some((c) => c.path === '/mod/ask/done' && c.body.id === 'tu1')).toBe(true)
})

test("asks again after each hold, and takes the terminal's answer when it comes first", async ($, on) => {
  mock.env(on, ENV)
  let asks = 0
  let third = () => undefined as void
  const thirdAsk = new Promise<void>((r) => (third = r))
  const calls = routes(on, (path) => {
    if (path !== '/mod/ask') return answer(204)
    if (asks++ < 2) return answer(204)
    third()
    return held()
  })
  const d = dialog(on)
  const call = $.tool.call({ tool: 'AskUserQuestion', tool_use_id: 'tu2', questions: QUESTIONS })
  await d.opened
  await thirdAsk
  const terminal = { result: { questions: QUESTIONS, answers: { 'Which color?': 'Red' } } }
  d.answer(terminal)
  const r = await call
  expect(r.result).toEqual(terminal.result)
  expect(calls.filter((c) => c.path === '/mod/ask').length).toBe(3)
  expect(calls.some((c) => c.path === '/mod/ask/done' && c.body.id === 'tu2')).toBe(true)
})

test("leaves the question to the terminal when AgentK can't take it", async ($, on) => {
  mock.env(on, ENV)
  const calls = routes(on, (path) => (path === '/mod/ask' ? answer(400, { error: "question 1 isn't a choice" }) : answer(204)))
  const d = dialog(on)
  const call = $.tool.call({ tool: 'AskUserQuestion', tool_use_id: 'tu3', questions: QUESTIONS })
  await d.opened
  d.answer({ result: { questions: QUESTIONS, answers: { 'Which color?': 'Red' } } })
  expect((await call).result).toEqual({ questions: QUESTIONS, answers: { 'Which color?': 'Red' } })
  expect(calls.filter((c) => c.path === '/mod/ask').length).toBe(1)
})

test('asks again a while after the server was away', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  let asks = 0
  on('http.fetch', (_$, e) => {
    const path = new URL(e.url).pathname
    if (path !== '/mod/ask') return answer(204)
    if (asks++ === 0) throw new Error('connect ENOENT')
    return answer(200, { answers: { 0: 'Red' } })
  })
  dialog(on)
  const call = $.tool.call({ tool: 'AskUserQuestion', tool_use_id: 'tu4', questions: QUESTIONS })
  await clock.advance(10)
  expect(asks).toBe(1)
  await clock.advance(3000)
  expect((await call).result).toEqual({ questions: QUESTIONS, answers: { 'Which color?': 'Red' } })
})

test("leaves Claude's questions to the terminal outside an AgentK session", async ($, on) => {
  mock.env(on, {})
  const calls = routes(on, () => answer(204))
  const d = dialog(on)
  const call = $.tool.call({ tool: 'AskUserQuestion', tool_use_id: 'tu5', questions: QUESTIONS })
  await d.opened
  d.answer({ result: { questions: QUESTIONS, answers: { 'Which color?': 'Red' } } })
  expect((await call).result).toEqual({ questions: QUESTIONS, answers: { 'Which color?': 'Red' } })
  expect(calls).toEqual([])
})

const DESIGN_QUESTIONS = [{ id: 'store', question: 'Where do designs live?', options: [{ label: 'Datapack' }, { label: 'Repo' }], recommended: 0, status: 'open' }]

test('gives Claude the design_questions tool, and passes its questions to AgentK', async ($, on) => {
  mock.env(on, ENV)
  mock.clock(on)
  const registered: string[] = []
  on('tool.register', (_$, e) => {
    registered.push(e.name)
    return { value: { tool: `mcp__agentk__${e.name}` } }
  })
  const calls = routes(on, (path) => (path === '/mod/questions' ? answer(200, { text: 'Saved 1 question (1 open).' }) : answer(204)))
  engine(on)
  await $.session.start(START)
  expect(registered).toEqual(['design_questions'])
  const r = await $.tool.call({ tool: 'mcp__agentk__design_questions', questions: DESIGN_QUESTIONS })
  expect(r.result).toBe('Saved 1 question (1 open).')
  expect(calls.find((c) => c.path === '/mod/questions')?.body).toEqual({ questions: DESIGN_QUESTIONS })
})

test("answers a bad design_questions call with what's wrong, for Claude to fix", async ($, on) => {
  mock.env(on, ENV)
  routes(on, (path) => (path === '/mod/questions' ? answer(400, { error: 'Question 1 needs an "id".' }) : answer(204)))
  const r = await $.tool.call({ tool: 'mcp__agentk__design_questions', questions: [{ question: 'Where?' }] })
  expect(r.deny ?? r.text).toBe('Question 1 needs an "id".')
})

test("tells AgentK when each of Claude's Meko calls starts and ends, failed ones too, and no other tool's", async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  const calls = routes(on, () => answer(204))
  // Meko's MCP server and Bash, beneath the mod. The failing call must run once, as without the mod.
  on('tool.call', { tool: 'mcp__meko__context_search' }, () => ({ result: 'found 3' }))
  let adds = 0
  on('tool.call', { tool: 'mcp__plugin_x_meko__memory_add' }, () => {
    adds++
    throw new Error('Meko is down')
  })
  on('tool.call', { tool: 'Bash' }, () => ({ result: 'ok' }))
  const r = await $.tool.call({ tool: 'mcp__meko__context_search', tool_use_id: 'tm1', query: 'login' })
  expect(r.result).toBe('found 3')
  // The hook's `.catch` replays what its `next(e)` settled to; nothing beneath runs again.
  await $.tool.call({ tool: 'mcp__plugin_x_meko__memory_add', tool_use_id: 'tm2', text: 'a fact' }).catch(() => null)
  expect(adds).toBe(1)
  await $.tool.call({ tool: 'Bash', tool_use_id: 'tb1', command: 'ls' })
  await clock.advance(1)
  expect(calls).toEqual([
    { path: '/mod/meko', body: { id: 'tm1', state: 'start' } },
    { path: '/mod/meko', body: { id: 'tm1', state: 'end' } },
    { path: '/mod/meko', body: { id: 'tm2', state: 'start' } },
    { path: '/mod/meko', body: { id: 'tm2', state: 'end' } },
  ])
})

test("leaves Claude's Meko calls alone outside an AgentK session", async ($, on) => {
  mock.env(on, {})
  const calls = routes(on, () => answer(204))
  on('tool.call', { tool: 'mcp__meko__context_search' }, () => ({ result: 'found 3' }))
  expect((await $.tool.call({ tool: 'mcp__meko__context_search', tool_use_id: 'tm3', query: 'login' })).result).toBe('found 3')
  expect(calls).toEqual([])
})

test('tells AgentK which prompts Claude still holds, until each starts its turn', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  const calls = server(on, [[{ id: 'p1', text: 'Revise the design.' }]])
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  // Claude holds the prompt (a turn is running) until `start`.
  let start = () => undefined as void
  on('prompt.submit', (_$, e) => new Promise((resolve) => (start = () => resolve({ text: e.text }))))
  await $.session.start(START)
  await clock.advance(5)
  const polls = () => calls.filter((c) => c.path === '/mod/poll').map((c) => c.body)
  expect(polls()[0]).toEqual({ held: [], protocol: 2 })
  expect(polls().at(-1)).toEqual({ held: ['p1'], protocol: 2 })
  start()
  await clock.advance(5)
  expect(calls.filter((c) => c.path === '/mod/held').map((c) => c.body)).toEqual([{ held: [] }])
  expect(polls().at(-1)).toEqual({ held: [], protocol: 2 })
})

const USAGE = { input_tokens: 12, output_tokens: 300, cache_read_input_tokens: 150000, cache_creation_input_tokens: 0 }

/**
 * The engine's model calls and toasts for a learn step: `fork` and `check` answer the fork and the
 * duplicate check; what each was asked, and the toasts, are kept.
 */
function models(on: On, fork: unknown, check: unknown) {
  const asked: { fork: string[]; check: { model: string; prompt: string }[]; toasts: string[] } = { fork: [], check: [], toasts: [] }
  on('session.model', () => ({ value: 'claude-opus-5-5' }))
  on('model.fork', (_$, e) => {
    asked.fork.push(e.prompt)
    return { value: fork as never }
  })
  on('model.complete', (_$, e) => {
    asked.check.push({ model: e.model, prompt: e.prompt })
    return { value: check as never }
  })
  on('ui.toast', (_$, e) => {
    asked.toasts.push(e.text)
    return { value: undefined }
  })
  return asked
}

test('runs a learn step: forks the session, runs the duplicate check AgentK hands over, and shows what was saved', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  // AgentK's side: each part of the step, then the line saying what it saved, comes on a poll.
  const answers = [
    { prompts: [], learn: { id: 'L1', kind: 'extract', prompt: 'What did you learn?' } },
    { prompts: [], learn: { id: 'L1', kind: 'check', prompt: 'Check these.', model: 'claude-haiku-4-5-20251001' } },
    { prompts: [], toast: 'Saved 1 memory and corrected 0 in team/demo.' },
  ]
  const calls = routes(on, (path) => {
    if (path === '/mod/poll') {
      const next = answers.shift()
      return next ? answer(200, next) : answer(204)
    }
    return answer(204) // /mod/learn answers at once
  })
  const asked = models(on, { isAnswered: true, text: '{"facts":[]}', usage: USAGE }, { isAnswered: true, text: '{"decisions":[]}', usage: USAGE })
  engine(on)
  await $.session.start(START)
  await clock.advance(10)
  expect(asked.fork).toEqual(['What did you learn?'])
  expect(asked.check).toEqual([{ model: 'claude-haiku-4-5-20251001', prompt: 'Check these.' }])
  expect(calls.filter((c) => c.path === '/mod/learn').map((c) => c.body)).toEqual([
    { id: 'L1', step: 'facts', model: 'claude-opus-5-5', text: '{"facts":[]}', usage: USAGE },
    { id: 'L1', step: 'decisions', model: 'claude-haiku-4-5-20251001', text: '{"decisions":[]}', usage: USAGE },
  ])
  expect(asked.toasts).toEqual(['Saved 1 memory and corrected 0 in team/demo.'])
})

test('tells AgentK why a fork gave nothing', async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  let polled = false
  const calls = routes(on, (path) => {
    if (path === '/mod/poll' && !polled) {
      polled = true
      return answer(200, { prompts: [], learn: { id: 'L2', kind: 'extract', prompt: 'What did you learn?' } })
    }
    return answer(204)
  })
  const asked = models(on, { isAnswered: false, reason: 'nothing-to-fork' }, null)
  engine(on)
  await $.session.start(START)
  await clock.advance(5)
  expect(calls.filter((c) => c.path === '/mod/learn').map((c) => c.body))
    .toEqual([{ id: 'L2', step: 'facts', model: 'claude-opus-5-5', reason: 'nothing-to-fork', usage: null }])
  expect(asked.check).toEqual([])
})

test('gives the terminal /agentk_learn, which asks AgentK to start a learn step', async ($, on) => {
  mock.env(on, ENV)
  mock.clock(on)
  const registered: string[] = []
  on('command.register', (_$, e) => {
    registered.push(e.name)
    return { value: { command: e.name } }
  })
  const calls = routes(on, (path) => (path === '/mod/learn/start' ? answer(200, { text: 'AgentK is saving what this session learned to Meko.' }) : answer(204)))
  engine(on)
  await $.session.start(START)
  expect(registered).toEqual(['agentk_learn'])
  const r = await $.command.run({ command: 'agentk_learn', args: '', origin: { kind: 'composer' }, presentation: { isFullscreen: false, columns: 120 } })
  expect(r.text).toBe('AgentK is saving what this session learned to Meko.')
  expect(calls.some((c) => c.path === '/mod/learn/start')).toBe(true)
})

test("tells AgentK at once when this Claude Code can't fork the session", async ($, on) => {
  mock.env(on, ENV)
  const clock = mock.clock(on)
  let polled = false
  const calls = routes(on, (path) => {
    if (path === '/mod/poll' && !polled) {
      polled = true
      return answer(200, { prompts: [], learn: { id: 'L3', kind: 'extract', prompt: 'What did you learn?' } })
    }
    return answer(204)
  })
  on('session.model', () => ({ value: 'claude-opus-5-5' }))
  on('model.fork', () => {
    throw new Error('not in this build')
  })
  engine(on)
  await $.session.start(START)
  await clock.advance(5)
  expect(calls.filter((c) => c.path === '/mod/learn').map((c) => c.body))
    .toEqual([{ id: 'L3', step: 'facts', model: 'claude-opus-5-5', reason: 'the model call failed' }])
})
