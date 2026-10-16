// Keyboard shortcuts the user can change: the actions, their defaults, and the bindings this browser keeps.
import { useEffect, useState } from 'react';

export type ShortcutAction =
  | 'toggle-projects'
  | 'palette'
  | 'next-session'
  | 'prev-session'
  | 'new-session'
  | 'rename-session'
  | 'move-session'
  | 'project-home'
  | 'usage'
  | 'settings';

export const SHORTCUT_ACTIONS: { action: ShortcutAction; label: string }[] = [
  { action: 'toggle-projects', label: 'Hide or show the projects pane' },
  { action: 'palette', label: 'Ask AgentK (the command palette)' },
  { action: 'next-session', label: 'Next session' },
  { action: 'prev-session', label: 'Previous session' },
  { action: 'new-session', label: 'New session' },
  { action: 'rename-session', label: 'Rename the session' },
  { action: 'move-session', label: 'Move the session to another stage' },
  { action: 'project-home', label: 'Project home' },
  { action: 'usage', label: 'Usage' },
  { action: 'settings', label: 'Settings' },
];

/** A binding is `Ctrl+Alt+Shift+Meta+Key`, modifiers in that order; null means no shortcut. */
export type Bindings = Record<ShortcutAction, string | null>;

const MODS = ['Ctrl', 'Alt', 'Shift', 'Meta'] as const;

// Ctrl+B is Claude Code's "run in the background", so off a Mac the pane takes Ctrl+Shift+B.
export function defaultBindings(mac: boolean): Bindings {
  return {
    'toggle-projects': mac ? 'Meta+B' : 'Ctrl+Shift+B',
    palette: mac ? 'Meta+K' : 'Ctrl+K',
    'next-session': 'Ctrl+Tab',
    'prev-session': 'Ctrl+Shift+Tab',
    'new-session': null,
    'rename-session': null,
    'move-session': null,
    'project-home': null,
    usage: null,
    settings: null,
  };
}

type KeyLike = Pick<KeyboardEvent, 'key' | 'code' | 'ctrlKey' | 'altKey' | 'shiftKey' | 'metaKey'>;

/** The binding a key press makes, or null for a modifier alone. */
export function eventBinding(e: KeyLike): string | null {
  if (['Control', 'Alt', 'Shift', 'Meta', 'OS', 'AltGraph', 'CapsLock', 'Dead', 'Unidentified', ''].includes(e.key)) return null;
  // The character typed, so ⌘K is K on any layout (Dvorak's K is KeyV). Only when Shift, Option or a
  // non-Latin layout turned the key into something else (⇧1 is !, ⌥B is ∫) does the physical key name it.
  const m = /^(?:Key([A-Z])|Digit([0-9]))$/.exec(e.code);
  const key = /^[a-z0-9]$/i.test(e.key)
    ? e.key.toUpperCase()
    : m
      ? (m[1] ?? m[2])
      : e.key === ' '
        ? 'Space'
        : e.key.length === 1
          ? e.key.toUpperCase()
          : e.key;
  const held = [e.ctrlKey, e.altKey, e.shiftKey, e.metaKey];
  return [...MODS.filter((_, i) => held[i]), key].join('+');
}

export function actionFor(bindings: Bindings, binding: string | null): ShortcutAction | null {
  if (!binding) return null;
  return SHORTCUT_ACTIONS.find((a) => bindings[a.action] === binding)?.action ?? null;
}

const MAC_SYMBOL: Record<string, string> = { Ctrl: '⌃', Alt: '⌥', Shift: '⇧', Meta: '⌘' };
const KEY_NAME: Record<string, string> = { ArrowUp: '↑', ArrowDown: '↓', ArrowLeft: '←', ArrowRight: '→', Escape: 'Esc' };

/** How a binding reads: `⌘B` or `⌃⇧Tab` on a Mac, `Ctrl+Shift+B` elsewhere. */
export function formatBinding(binding: string, mac: boolean): string {
  const parts = binding.split('+');
  // A binding of the + key itself ends in an empty part.
  const key = parts.at(-1) || '+';
  const mods = parts.slice(0, -1).filter(Boolean);
  const name = KEY_NAME[key] ?? key;
  if (mac) return mods.map((m) => MAC_SYMBOL[m] ?? m).join('') + name;
  return [...mods.map((m) => (m === 'Meta' ? 'Win' : m)), name].join('+');
}

export function ariaBinding(binding: string): string {
  return binding.split('+').map((p) => (p === 'Ctrl' ? 'Control' : p)).join('+');
}

/**
 * Why a key press can't be a shortcut, or null when it can. A bound key never reaches the terminal,
 * so a shortcut can't be a key the terminal takes (a plain key, Ctrl+letter, or Alt/Option with a
 * typed key), nor one the app keeps for itself.
 */
export function bindingProblem(binding: string, mac: boolean): string | null {
  const parts = binding.split('+');
  const key = parts.at(-1) || '+';
  const mods = new Set(parts.slice(0, -1).filter(Boolean));
  const shown = formatBinding(binding, mac);
  const typed = key.length === 1;
  if (!mods.has('Ctrl') && !mods.has('Alt') && !mods.has('Meta') && !/^F\d{1,2}$/.test(key)) {
    return `Add ${mac ? '⌘' : 'Ctrl'}, so typing still reaches the terminal.`;
  }
  const mod = mac ? 'Meta' : 'Ctrl';
  if (mods.size === 1 && mods.has(mod) && /^[1-9]$/.test(key)) return `${shown} switches projects.`;
  if (mods.has(mod) && !mods.has('Alt') && ['C', 'V', 'X', 'A', 'Z'].includes(key)) return `${shown} is kept for editing text.`;
  if (key === 'Enter' && (mods.has('Ctrl') || mods.has('Meta')) && mods.size === 1) return `${shown} sends dialogs.`;
  if (typed && mods.has('Alt') && !mods.has('Ctrl') && !mods.has('Meta')) {
    return mac ? `${shown} types a character on a Mac. Add ⌘ or ⌃.` : `${shown} goes to the terminal. Add Ctrl.`;
  }
  if (/^[A-Z]$/.test(key) && mods.has('Ctrl') && !mods.has('Shift') && !mods.has('Alt') && !mods.has('Meta')) {
    return `${shown} goes to the terminal (Claude uses Ctrl+letter keys). Add ${mac ? '⇧ or ⌥' : 'Shift or Alt'}.`;
  }
  return null;
}

/** The saved bindings over the defaults; anything unreadable is left at its default. */
export function mergeBindings(defaults: Bindings, saved: unknown): Bindings {
  const out = { ...defaults };
  if (!saved || typeof saved !== 'object') return out;
  for (const { action } of SHORTCUT_ACTIONS) {
    const v = (saved as Record<string, unknown>)[action];
    if (v === null || typeof v === 'string') out[action] = v || null;
  }
  return out;
}

/** `bindings` with `action` on `binding`, taken off any other action that had it. */
export function assign(bindings: Bindings, action: ShortcutAction, binding: string | null): Bindings {
  const out = { ...bindings, [action]: binding };
  if (binding) for (const { action: a } of SHORTCUT_ACTIONS) if (a !== action && out[a] === binding) out[a] = null;
  return out;
}

export function changedBindings(defaults: Bindings, bindings: Bindings): Partial<Bindings> {
  const out: Partial<Bindings> = {};
  for (const { action } of SHORTCUT_ACTIONS) if (bindings[action] !== defaults[action]) out[action] = bindings[action];
  return out;
}

const KEY = 'ak-shortcuts';
const CHANGED = 'ak-shortcuts-changed';
const isMacBrowser = () => /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

let cached: Bindings | null = null;

export function shortcuts(): Bindings {
  if (cached) return cached;
  let saved: unknown = null;
  try {
    saved = JSON.parse(localStorage.getItem(KEY) ?? 'null');
  } catch {
    /* unreadable or no storage: the defaults */
  }
  cached = mergeBindings(defaultBindings(isMacBrowser()), saved);
  return cached;
}

/** Saves this browser's bindings (only the changes from the defaults) and tells the page. */
export function saveShortcuts(bindings: Bindings): void {
  cached = bindings;
  const changed = changedBindings(defaultBindings(isMacBrowser()), bindings);
  try {
    if (Object.keys(changed).length) localStorage.setItem(KEY, JSON.stringify(changed));
    else localStorage.removeItem(KEY);
  } catch {
    /* storage unavailable: they hold for this page */
  }
  window.dispatchEvent(new Event(CHANGED));
}

export function resetShortcuts(): void {
  saveShortcuts(defaultBindings(isMacBrowser()));
}

export function shortcutAction(e: KeyboardEvent): ShortcutAction | null {
  return actionFor(shortcuts(), eventBinding(e));
}

export function shortcutLabel(binding: string | null): string | null {
  return binding && formatBinding(binding, isMacBrowser());
}

/** This browser's bindings, kept current as they change here or in another tab. */
export function useShortcuts(): Bindings {
  const [b, setB] = useState(shortcuts);
  useEffect(() => {
    const update = () => setB(shortcuts());
    const onStorage = (e: StorageEvent) => {
      if (e.key !== KEY && e.key !== null) return;
      cached = null;
      update();
    };
    window.addEventListener(CHANGED, update);
    window.addEventListener('storage', onStorage);
    return () => {
      window.removeEventListener(CHANGED, update);
      window.removeEventListener('storage', onStorage);
    };
  }, []);
  return b;
}
