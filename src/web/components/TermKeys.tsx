import { useEffect, useRef, useState, type SyntheticEvent } from 'react';
import { BAR_KEYS } from '../keys';
import type { SessionTerminal } from '../terminals';
import { cx } from '../util';

const draftKey = (id: string) => `ak-compose-${id}`;

function readDraft(id: string): string {
  try {
    return sessionStorage.getItem(draftKey(id)) ?? '';
  } catch {
    return '';
  }
}

function saveDraft(id: string, text: string) {
  try {
    if (text) sessionStorage.setItem(draftKey(id), text);
    else sessionStorage.removeItem(draftKey(id));
  } catch {
    /* storage blocked: the draft just isn't kept */
  }
}

/** Keeps focus (and the on-screen keyboard) where it is when a bar button is tapped. */
const keepFocus = (e: SyntheticEvent) => e.preventDefault();

/**
 * For touch screens: keys a phone keyboard lacks, and a compose box that gets the phone's
 * autocorrect, dictation and paste. The terminal font size is in the session sheet.
 */
export function TermKeys({ term, sessionId, disabled }: { term: SessionTerminal; sessionId: string; disabled: boolean }) {
  const [text, setText] = useState(() => readDraft(sessionId));
  const box = useRef<HTMLTextAreaElement>(null);

  useEffect(() => saveDraft(sessionId, text), [sessionId, text]);

  // Grow with the text, up to the height CSS allows.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${el.scrollHeight + 2}px`; // + borders
  }, [text]);

  const send = () => {
    if (!text.trim() || disabled) return;
    term.submit(text);
    setText('');
  };

  return (
    <div className="term-keys">
      <div className="keybar" role="toolbar" aria-label="Terminal keys">
        {BAR_KEYS.map((k) => (
          <button
            key={k.key}
            className="key"
            title={k.title}
            aria-label={k.title}
            disabled={disabled}
            onPointerDown={keepFocus}
            onMouseDown={keepFocus}
            onClick={() => term.sendKey(k.key)}
          >
            {k.label}
          </button>
        ))}
        <button
          className={cx('key', term.typing && 'on')}
          aria-pressed={term.typing}
          title={term.typing ? 'Stop typing into the terminal' : 'Type straight into the terminal'}
          aria-label="Type straight into the terminal"
          disabled={disabled}
          onPointerDown={keepFocus}
          onMouseDown={keepFocus}
          onClick={() => term.setTyping(!term.typing)}
        >
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <rect x="2" y="6" width="20" height="12" rx="2" />
            <path d="M6 10h.01 M10 10h.01 M14 10h.01 M18 10h.01 M7 14h10" />
          </svg>
        </button>
      </div>
      <form
        className="compose"
        onSubmit={(e) => {
          e.preventDefault();
          send();
        }}
      >
        <textarea
          ref={box}
          id={`compose-${sessionId}`}
          className="compose-input"
          rows={1}
          placeholder={disabled ? 'Resume the session to message Claude' : 'Message Claude…'}
          aria-label="Message Claude"
          value={text}
          disabled={disabled}
          enterKeyHint="enter"
          onChange={(e) => setText(e.target.value)}
          onFocus={() => {
            if (term.typing) term.setTyping(false);
            // Claude's prompt stays in view above the box while the keyboard is up.
            term.scrollToBottom();
          }}
          onKeyDown={(e) => {
            // A hardware keyboard can send with Ctrl/⌘+Enter; plain Enter adds a line.
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
              e.preventDefault();
              send();
            }
          }}
        />
        <button
          type="submit"
          className="compose-send"
          aria-label="Send to Claude"
          title="Send to Claude"
          disabled={disabled || !text.trim()}
          onPointerDown={keepFocus}
          onMouseDown={keepFocus}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d="M12 19V5 M5 12l7-7 7 7" />
          </svg>
        </button>
      </form>
    </div>
  );
}
