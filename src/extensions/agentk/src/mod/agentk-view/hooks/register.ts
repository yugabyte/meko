// Kode's view mod: draws Kode's prompts in the terminal of each Claude Code session Kode runs
// as one line, "<first line> … (+N lines, ctrl+o to expand)". The full transcript (ctrl+o) shows them
// in full, and Claude reads them whole: only the row's drawing changes.
//
// It's a plugin of its own, loaded beside Kode's mod (src/mod/agentk) with a second --plugin-dir,
// because Claude Code never raises a plugin's `ui.render` hook for the rows of prompts that same
// plugin submitted: Kode's mod, which submits them, can't draw them.
//
// Which rows are Kode's:
//   - a prompt Kode's mod submitted: origin `{ kind: 'plugin', name: 'kode' }`, which the
//     transcript keeps, so a resumed session draws it the same way;
//   - the session's first prompt, which Kode passes on claude's command line: Claude Code draws it
//     like typed text, so the server names its first line in KODE_FIRST_PROMPT at every launch
//     (empty when the conversation didn't start with Kode's prompt).
// A prompt Kode typed into the terminal (while its mod wasn't linked) draws as Claude Code draws it.
//
// It must never get in Claude's way: a row it can't read draws as Claude Code draws it.
//
// This file is compiled by Claude Code, not by the repo's tsc: `claude-code` is the engine's own
// module, typed by the declarations Claude Code writes into .claude-plugin/types when it loads the mod.
import type { EngineInterface, Register } from 'claude-code'

/** The plugin that submits Kode's prompts (src/mod/agentk). */
const SENDER = 'kode'
/** The most of a first line a drawn row keeps. */
const LINE_MAX = 200
interface Marks {
  /** The first line of the conversation's first prompt, when Kode sent it; '' otherwise. */
  first: string
}

let marks: Promise<Marks> | null = null

function marksOf($: EngineInterface): Promise<Marks> {
  marks ??= (async () => ({
    first: ((await $.env.get('KODE_FIRST_PROMPT')) ?? (await $.env.get('AK_FIRST_PROMPT')) ?? '').trim(),
  }))()
  return marks
}

/** A prompt's first line that has text, trimmed: as `firstLine()` in src/server/prompts.ts reads it. */
function firstLine(text: string): string {
  return text.split('\n').map((l) => l.trim()).find(Boolean) ?? ''
}

/** The one line a prompt draws as, or null when it already fits on one. */
function folded(text: string): string | null {
  const line = firstLine(text)
  const more = text.trim().split('\n').length - 1
  if (!more && line.length <= LINE_MAX) return null
  const head = line.length > LINE_MAX ? `${line.slice(0, LINE_MAX - 1)}…` : line
  return more ? `${head} … (+${more} ${more === 1 ? 'line' : 'lines'}, ctrl+o to expand)` : `${head} (ctrl+o to expand)`
}

export const register: Register = (on) => {
  // Registered in a try: a Claude Code without the hook still loads the mod, and draws every row itself.
  try {
    on('ui.render', { component: 'UserMessage' }, async ($, e, next) => {
      const p = e.props as { text?: unknown; origin?: { kind?: unknown; name?: unknown }; isExpanded?: unknown }
      if (p.isExpanded || typeof p.text !== 'string') return next(e)
      const m = await marksOf($)
      const line = firstLine(p.text)
      const sent = p.origin?.kind === 'plugin' && p.origin.name === SENDER
      const first = p.origin?.kind !== 'plugin' && !!m.first && line === m.first
      const text = sent || first ? folded(p.text) : null
      return next(text === null ? e : { ...e, props: { ...e.props, text } } as typeof e)
    }).catch(($, e, next) => next(e))
  } catch {
    // The rows draw as Claude Code draws them
  }
}
