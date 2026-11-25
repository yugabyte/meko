// Run with `claude plugin test src/mod/agentk-view` (the repo's vitest suite runs it when Claude Code
// is installed: test/unit/mod.test.ts).
import { expect, mock, test } from 'claude-code/testing'
import type { EngineInterface, On } from 'claude-code'

const STAGE = 'You are in the Code stage of a Kode task.\n\nTask: Fix the score\n\nRun the tests.'
const FIRST = 'You are in the Explore stage of a Kode task.\n\nTask: Explore\n\nRead the code.'
const ENV = { KODE_FIRST_PROMPT: 'You are in the Explore stage of a Kode task.' }

const AGENTK = { kind: 'plugin', name: 'kode', asUser: true } as const
const TYPED = { kind: 'composer' } as const

type Origin = typeof AGENTK | typeof TYPED | { kind: 'plugin'; name: string; asUser: boolean }

/** The engine's own drawing of a row beneath the mod: a Text of the text it was handed. */
function drawn(on: On) {
  on('ui.render', (_$, e) => ({ type: 'Text', children: [(e.props as { text: string }).text] }))
}

function row($: EngineInterface, text: string, origin: Origin, isExpanded = false) {
  return $.ui.render({ surface: 'terminal', component: 'UserMessage', requestId: `u-${text.length}-${origin.kind}`, props: { text, origin, isExpanded } })
}

const text = (t: string) => expect.objectContaining({ type: 'Text', children: [t] })

test("draws Kode's prompts as their first line, and in full with ctrl+o", async ($, on) => {
  mock.env(on, ENV)
  drawn(on)
  expect(await row($, STAGE, AGENTK)).toEqual(text('You are in the Code stage of a Kode task. … (+4 lines, ctrl+o to expand)'))
  expect(await row($, STAGE, AGENTK, true)).toEqual(text(STAGE))
  // One line already: as it is.
  expect(await row($, 'Implement the design.', AGENTK)).toEqual(text('Implement the design.'))
  // A first line too long for the row is cut.
  const long = 'x'.repeat(300)
  expect(await row($, long, AGENTK)).toEqual(text(`${'x'.repeat(199)}… (ctrl+o to expand)`))
})

test("draws the session's first prompt, passed on claude's command line, as its first line", async ($, on) => {
  mock.env(on, ENV)
  drawn(on)
  expect(await row($, FIRST, TYPED)).toEqual(text('You are in the Explore stage of a Kode task. … (+4 lines, ctrl+o to expand)'))
  expect(await row($, FIRST, TYPED, true)).toEqual(text(FIRST))
})

test("leaves the person's own prompts, and other plugins', as Claude Code draws them", async ($, on) => {
  mock.env(on, ENV)
  drawn(on)
  const mine = 'Why does the score\ndrop to zero?'
  expect(await row($, mine, TYPED)).toEqual(text(mine))
  expect(await row($, STAGE, TYPED)).toEqual(text(STAGE))
  expect(await row($, STAGE, { kind: 'plugin', name: 'other', asUser: true })).toEqual(text(STAGE))
})

for (const [why, env] of [['empty', { KODE_FIRST_PROMPT: '' }], ['unset', {}]] as const) {
  test(`folds no typed prompt when the conversation didn't start with Kode's (KODE_FIRST_PROMPT ${why})`, async ($, on) => {
    mock.env(on, env)
    drawn(on)
    expect(await row($, FIRST, TYPED)).toEqual(text(FIRST))
    // Kode's mod's prompts still fold.
    expect(await row($, STAGE, AGENTK)).toEqual(text('You are in the Code stage of a Kode task. … (+4 lines, ctrl+o to expand)'))
  })
}
