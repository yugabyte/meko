import { useEffect, useRef, useState } from 'react';
import { pastedFiles } from '../clipboard';
import { BAR_KEYS, MORE_KEYS, insertedChar, type BarKey } from '../keys';
import type { SessionTerminal } from '../terminals';
import { cx, keepFocus } from '../util';
import { HeldImages } from './HeldImages';
import { MekoUpload } from './MekoUpload';

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

/** A bar key that sends its bytes to the terminal. */
function SendKey({ term, k, disabled }: { term: SessionTerminal; k: { key: BarKey; label: string; title: string }; disabled: boolean }) {
  return (
    <button
      className="key"
      title={k.title}
      aria-label={k.title}
      disabled={disabled}
      onPointerDown={keepFocus}
      onMouseDown={keepFocus}
      onClick={() => {
        if (!term.sendKey(k.key)) term.showNotice(term.cantType(), 3000);
      }}
    >
      {k.label}
    </button>
  );
}

/**
 * For touch screens: keys a phone keyboard lacks, and a compose box that gets the phone's
 * autocorrect, dictation and paste. Images and files (pasted into the box, or picked with the attach key
 * in the ▾ row) wait above the box and go with the message. The terminal font size is in the session sheet.
 */
/** `mekoTo`: the datapack Upload to Meko sends held files to, or null when there's none (Meko off, or no datapack). */
export function TermKeys({ term, sessionId, disabled, mekoTo }: { term: SessionTerminal; sessionId: string; disabled: boolean; mekoTo: string | null }) {
  const [text, setText] = useState(() => readDraft(sessionId));
  /** The row of more keys (Ctrl, Esc, Shift+Tab…) is open. */
  const [more, setMore] = useState(false);
  const box = useRef<HTMLTextAreaElement>(null);
  const picker = useRef<HTMLInputElement>(null);

  useEffect(() => saveDraft(sessionId, text), [sessionId, text]);

  // Grow with the text, up to the height CSS allows.
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${el.scrollHeight + 2}px`; // + borders
  }, [text]);

  const held = term.held.size > 0;
  const canSend = (!!text.trim() || held) && !disabled && !term.sending;

  /**
   * Sends the text and any held images. Returns whether sending started. The text comes back to the
   * box if none of it reached Claude (the terminal says why), ahead of anything typed meanwhile.
   */
  const send = () => {
    if (!canSend) return false;
    if (!term.canType) {
      term.showNotice(term.cantType(true), 4000);
      return false;
    }
    const sent = text.trim() ? text : '';
    setText('');
    void term.submit(sent).then((ok) => {
      if (!ok && sent) setText((t) => (t ? `${sent}\n${t}` : sent));
    });
    return true;
  };

  return (
    <div className="term-keys">
      <div className="keybar" role="toolbar" aria-label="Terminal keys">
        {BAR_KEYS.map((k) => (
          <SendKey key={k.key} term={term} k={k} disabled={disabled} />
        ))}
        <button
          className={cx('key', term.selecting && 'on')}
          aria-pressed={term.selecting}
          title={term.selecting ? 'Stop selecting' : 'Select lines to copy: drag over them'}
          aria-label="Select lines to copy"
          onPointerDown={keepFocus}
          onMouseDown={keepFocus}
          onClick={() => term.setSelecting(!term.selecting)}
        >
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d="M4 6h10 M4 12h16 M4 18h12" />
            <path d="M19 3v6 M17 3h4 M17 9h4" />
          </svg>
        </button>
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
        <button
          // Lit while its row is open, or while Ctrl is armed with the row closed.
          className={cx('key', (more || term.ctrl) && 'on')}
          aria-expanded={more}
          // The row is only in the page while it's open.
          aria-controls={more ? `more-keys-${sessionId}` : undefined}
          title={more ? 'Fewer keys' : 'More keys: Ctrl, Esc, Shift+Tab, image…'}
          aria-label="More keys"
          // Its keys can't be used while the session is stopped, but an open row can still be closed.
          disabled={disabled && !more}
          onPointerDown={keepFocus}
          onMouseDown={keepFocus}
          onClick={() => setMore(!more)}
        >
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <path d={more ? 'M6 15l6-6 6 6' : 'M6 9l6 6 6-6'} />
          </svg>
        </button>
      </div>
      {more && (
        <div id={`more-keys-${sessionId}`} className="keybar" role="toolbar" aria-label="More terminal keys">
          <button
            className={cx('key', term.ctrl && 'on')}
            aria-pressed={term.ctrl}
            title="Ctrl: the next letter you type is sent with Ctrl, as in Ctrl+O or Ctrl+R"
            aria-label="Ctrl for the next letter you type"
            disabled={disabled}
            onPointerDown={keepFocus}
            onMouseDown={keepFocus}
            onClick={() => {
              const on = !term.ctrl;
              term.setCtrl(on);
              // Bring the keyboard up for the letter, unless the terminal already has it.
              if (on && !term.typing) box.current?.focus();
            }}
          >
            ctrl
          </button>
          {MORE_KEYS.map((k) => (
            <SendKey key={k.key} term={term} k={k} disabled={disabled} />
          ))}
          {/* A phone has no paste or drop of files into the terminal: pick them instead. */}
          <button
            className="key"
            title="Attach an image or file for Claude"
            aria-label="Attach an image or file for Claude"
            disabled={disabled}
            onPointerDown={keepFocus}
            onMouseDown={keepFocus}
            onClick={() => picker.current?.click()}
          >
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M21 12.5l-8.5 8.5a5 5 0 0 1-7-7l9-9a3.5 3.5 0 0 1 5 5l-9 9a2 2 0 0 1-3-3l8.5-8.5" />
            </svg>
          </button>
        </div>
      )}
      {/* Outside the ▾ row, so the picker's choice arrives whatever the row does meanwhile. */}
      <input
        ref={picker}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const files = Array.from(e.target.files ?? []);
          // Cleared, so picking the same file again still fires a change.
          e.target.value = '';
          if (!files.length) return;
          term.holdFiles(files);
          // Picked: the row has done its job, so give the terminal its space back.
          setMore(false);
        }}
      />
      <HeldImages held={term.held} hint="sent with your message" actions={mekoTo && <MekoUpload term={term} datapack={mekoTo} />} />
      <form
        className="compose"
        onSubmit={(e) => {
          e.preventDefault();
          // Tapping send puts the keyboard away, so the terminal gets the screen back for Claude's reply.
          if (send()) box.current?.blur();
        }}
      >
        <textarea
          ref={box}
          id={`compose-${sessionId}`}
          className="compose-input"
          // Its draft is kept in sessionStorage, so an update's reload doesn't wait on it (reload.ts).
          data-reload-safe=""
          rows={1}
          placeholder={disabled ? 'Resume the session to message Claude' : 'Message Claude…'}
          aria-label="Message Claude"
          value={text}
          disabled={disabled}
          enterKeyHint="enter"
          // An armed Ctrl takes one letter as it is: no autocorrect or capitals rewriting it.
          autoCorrect={term.ctrl ? 'off' : undefined}
          autoCapitalize={term.ctrl ? 'none' : undefined}
          onPaste={(e) => {
            // Images and files wait above the box and go with the message; text pastes into the box as usual.
            const files = pastedFiles(e.clipboardData);
            if (!files.length) return;
            e.preventDefault();
            term.holdFiles(files);
          }}
          onChange={(e) => {
            const next = e.target.value;
            // With Ctrl armed, a typed character goes to Claude as Ctrl+character instead of the box.
            const ch = term.ctrl ? insertedChar(text, next) : null;
            if (ch !== null) {
              if (term.typeCtrl(ch)) return;
              // Meant as Ctrl+letter but it can't reach Claude: say so rather than type the letter.
              if (!term.canType) {
                term.showNotice(term.cantType(), 3000);
                return;
              }
            }
            setText(next);
          }}
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
          disabled={!canSend}
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
