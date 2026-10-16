import { useState, type KeyboardEvent } from 'react';
import {
  SHORTCUT_ACTIONS,
  assign,
  bindingProblem,
  defaultBindings,
  eventBinding,
  resetShortcuts,
  saveShortcuts,
  shortcutLabel,
  useShortcuts,
  type ShortcutAction,
} from '../shortcuts';
import { isMac } from '../util';

/** Settings' Shortcuts tab: each action's keys in this browser, to change, remove or reset. */
export function ShortcutsTab() {
  const bindings = useShortcuts();
  const [recording, setRecording] = useState<ShortcutAction | null>(null);
  const [error, setError] = useState<{ action: ShortcutAction; text: string } | null>(null);
  const mac = isMac();
  const defaults = defaultBindings(mac);
  const mod = mac ? '⌘' : 'Ctrl+';
  const changed = SHORTCUT_ACTIONS.some(({ action }) => bindings[action] !== defaults[action]);

  const record = (action: ShortcutAction, e: KeyboardEvent<HTMLButtonElement>) => {
    e.preventDefault();
    e.stopPropagation();
    if (e.key === 'Escape' && !e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey) {
      setRecording(null);
      return;
    }
    const b = eventBinding(e.nativeEvent);
    if (!b) return;
    // Its own default is always allowed back (though Ctrl+K, say, breaks the rules for new keys), as Default does.
    const own = b === defaults[action];
    const taken = SHORTCUT_ACTIONS.find((a) => a.action !== action && bindings[a.action] === b);
    const text = own ? null : (bindingProblem(b, mac) ?? (taken && `${shortcutLabel(b)} already runs “${taken.label}”. Remove or change it there first.`));
    if (text) {
      setError({ action, text });
      return;
    }
    saveShortcuts(assign(bindings, action, b));
    setRecording(null);
    setError(null);
  };

  return (
    <div className="stack">
      <p className="hint">
        Kept in this browser. {mod}1–9 switch projects and {mod}Enter sends a dialog; those can't be changed.
      </p>
      <div className="shortcuts">
        {SHORTCUT_ACTIONS.map(({ action, label }) => {
          const b = bindings[action];
          const on = recording === action;
          return (
            <div className="shortcut-row" key={action}>
              <span className="shortcut-label">{label}</span>
              {on ? (
                <button
                  className="btn shortcut-rec"
                  data-own-keys
                  autoFocus
                  onKeyDown={(e) => record(action, e)}
                  onBlur={() => setRecording(null)}
                  onClick={() => setRecording(null)}
                >
                  Press the keys… (Esc cancels)
                </button>
              ) : (
                <span className="shortcut-keys">{b ? <kbd>{shortcutLabel(b)}</kbd> : <span className="muted">None</span>}</span>
              )}
              <span className="shortcut-actions">
                {!on && (
                  <button
                    type="button"
                    className="btn ghost"
                    onClick={() => {
                      setError(null);
                      setRecording(action);
                    }}
                  >
                    {b ? 'Change' : 'Add'}
                  </button>
                )}
                {!on && b && (
                  <button type="button" className="btn ghost" onClick={() => saveShortcuts({ ...bindings, [action]: null })}>
                    Remove
                  </button>
                )}
                {!on && b !== defaults[action] && defaults[action] && (
                  <button
                    type="button"
                    className="btn ghost"
                    title={`Back to ${shortcutLabel(defaults[action])}`}
                    onClick={() => saveShortcuts(assign(bindings, action, defaults[action]))}
                  >
                    Default
                  </button>
                )}
              </span>
              {error?.action === action && (
                <span className="shortcut-error error-text" role="alert">
                  {error.text}
                </span>
              )}
            </div>
          );
        })}
      </div>
      {changed && (
        <div>
          <button
            type="button"
            className="btn"
            onClick={() => {
              resetShortcuts();
              setError(null);
            }}
          >
            Reset all to the defaults
          </button>
        </div>
      )}
    </div>
  );
}
