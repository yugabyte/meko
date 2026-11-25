import { useEffect, useRef } from 'react';
import type { SessionTerminal } from '../terminals';
import { findLabel, isFindKey, type FindToggles } from '../termfind';
import { isMac } from '../util';

/** The last find request each terminal's bar answered, so a bar shown again (back on its session) doesn't take focus. */
const answered = new WeakMap<SessionTerminal, number>();

const TOGGLES: { key: keyof FindToggles; label: string; title: string }[] = [
  { key: 'caseSensitive', label: 'Aa', title: 'Match case' },
  { key: 'wholeWord', label: 'ab', title: 'Whole word' },
  { key: 'regex', label: '.*', title: 'Regular expression' },
];

/**
 * The find bar over the terminal (⌘F, Ctrl+Shift+F elsewhere, or Find in terminal from the ⋯ sheet
 * and the palette). Its state is the terminal's (`term.find`), so it outlasts the pane. Clicks stay
 * in the bar: the terminal's own click handler would focus it, and on another screen's session
 * that asks for the session.
 */
export function TermFind({ term, agent }: { term: SessionTerminal; agent: string }) {
  const input = useRef<HTMLInputElement>(null);
  const f = term.find;
  const label = findLabel(f.query, f.regex, f.result);

  // Opened, or asked for again while open: focus the field with its text selected, ready to retype.
  // Even on touch: the bar is opened in order to type.
  useEffect(() => {
    if (answered.get(term) === term.findFocus) return;
    // A frame later, so a palette or sheet closing as it opens find doesn't take the focus back.
    const frame = requestAnimationFrame(() => {
      answered.set(term, term.findFocus);
      input.current?.focus();
      input.current?.select();
    });
    return () => cancelAnimationFrame(frame);
  }, [term, term.findFocus]);

  return (
    <div className="term-find" role="search" onClick={(e) => e.stopPropagation()}>
      <div className="term-find-row">
        <input
          ref={input}
          className="term-find-input"
          type="text"
          aria-label="Find in terminal"
          placeholder="Find"
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          value={f.query}
          onChange={(e) => term.setFindQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              term.findStep(e.shiftKey ? 'newer' : 'older');
            } else if (e.key === 'Escape') {
              e.preventDefault();
              term.closeFind();
            } else if (isFindKey(e.nativeEvent, isMac())) {
              e.preventDefault();
              e.currentTarget.select();
            }
          }}
        />
        <span className="term-find-count" aria-live="polite">
          {label}
        </span>
      </div>
      <div className="term-find-row term-find-btns">
        {TOGGLES.map((t) => (
          <button
            key={t.key}
            type="button"
            className="term-find-btn toggle"
            title={t.title}
            aria-label={t.title}
            aria-pressed={f[t.key]}
            onClick={() => term.setFindToggle(t.key, !f[t.key])}
          >
            {t.label}
          </button>
        ))}
        <button type="button" className="term-find-btn" title="Older match (Enter)" aria-label="Older match" disabled={!f.result.count} onClick={() => term.findStep('older')}>
          ↑
        </button>
        <button type="button" className="term-find-btn" title="Newer match (Shift+Enter)" aria-label="Newer match" disabled={!f.result.count} onClick={() => term.findStep('newer')}>
          ↓
        </button>
        <button type="button" className="term-find-btn" title="Close (Esc)" aria-label="Close find" onClick={() => term.closeFind()}>
          ×
        </button>
      </div>
      {term.fullScreen && <div className="term-find-note">Searches only the screen while {agent} runs full screen.</div>}
    </div>
  );
}
