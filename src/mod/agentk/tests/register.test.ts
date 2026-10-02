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
  expect(calls[0]).toEqual({ path: '/mod/hello', body: { protocol: 1 }, socketPath: '/ak/hook.sock', token: 'tok' })
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

test('does nothing outside an AgentK session', async ($, on) => {
  mock.env(on, {})
  const clock = mock.clock(on)
  const calls = server(on)
  engine(on)
  await $.session.start(START)
  await $.session.measure({ context: { window: 200000 }, rateLimits: [], changed: ['context'] })
  await clock.advance(5000)
  expect(calls).toEqual([])
})
