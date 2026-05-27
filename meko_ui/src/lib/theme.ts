// Theme provider for the Meko app: reads the user's preferred mode from
// localStorage on mount (falling back to the OS-level
// `prefers-color-scheme` on first visit), applies it to `<html>` by
// toggling the `.dark` class, and exposes a stable `useTheme()` hook
// so any component can read the current mode + flip it.
//
// CSS variable scaffolding lives in `src/index.css`:
//   - `:root`     carries the LIGHT theme values
//   - `.dark { }` overrides every value for DARK theme
// All component-facing colour utilities (`bg-card`, `bg-input`,
// `text-foreground`, etc.) resolve via those CSS variables, so flipping
// the class is the only mechanism we need to re-theme the whole app.

import {
  useCallback,
  useEffect,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react";

/** Two-mode theme. We don't currently support an explicit "system" mode
 *  separate from light/dark — first-load picks based on OS preference
 *  and then the user's explicit choice is what we persist. Easy to add
 *  later if "Match system" becomes a desired option. */
export type Theme = "light" | "dark";

/** localStorage key. `meko:` prefix matches the other persisted-pref
 *  keys in the codebase (`meko:observe-bootstrap-cache`,
 *  `meko:observe-iframe-version`). Langfuse Observe embed reads the
 *  same key when served from the same origin via meko-proxy. */
export const THEME_STORAGE_KEY = "meko:theme";

/** postMessage `type` for parent → Langfuse iframe theme sync. */
export const THEME_MESSAGE_TYPE = "meko:theme";

/** CSS class toggled on `<html>` to activate the dark-mode variables.
 *  Tailwind's `dark:` variant + our `.dark { … }` block in
 *  `index.css` both key off this one class — keep them in sync. */
const DARK_CLASS = "dark";

/** Read the persisted choice, then fall back to the OS preference, then
 *  to `"dark"` as a final default (matches the legacy app behaviour —
 *  the app was dark-only before the theme refactor, so "dark" is the
 *  safest fallback if storage is unreadable). */
function getInitialTheme(): Theme {
  if (typeof window === "undefined") return "dark";
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    if (stored === "light" || stored === "dark") return stored;
  } catch {
    // private mode / denied — fall through to OS preference
  }
  if (window.matchMedia?.("(prefers-color-scheme: light)").matches) {
    return "light";
  }
  return "dark";
}

/** Notify embedded Langfuse iframes on the same origin (Observe hub). */
function broadcastThemeToEmbeds(theme: Theme): void {
  if (typeof document === "undefined") return;
  const payload = { type: THEME_MESSAGE_TYPE, theme };
  document.querySelectorAll("iframe").forEach((iframe) => {
    try {
      iframe.contentWindow?.postMessage(payload, window.location.origin);
    } catch {
      // iframe not ready or cross-origin
    }
  });
}

/** Apply the theme to `<html>` and persist the choice. Idempotent —
 *  safe to call from an effect on every render. */
function applyTheme(theme: Theme): void {
  if (typeof document === "undefined") return;
  const root = document.documentElement;
  if (theme === "dark") root.classList.add(DARK_CLASS);
  else root.classList.remove(DARK_CLASS);
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Storage unavailable — the in-memory state still works; just
    // won't survive a reload. Silent by design.
  }
  broadcastThemeToEmbeds(theme);
}

/** Sync the `<html>` class to the initial preference BEFORE React
 *  mounts so the first paint already has the right theme. Without
 *  this, a user on light mode would see a brief flash of dark
 *  before the `useTheme` effect runs.
 *
 *  Called once from `main.tsx` (or equivalent entry). Idempotent so
 *  it's safe to call again if the entry re-imports. */
export function initThemeBeforeMount(): void {
  applyTheme(getInitialTheme());
}

/**
 * Read the current theme + flip it.
 *
 * **Prefer CSS over this hook for visual differences.** 99% of the
 * time, "I need this to look different in light vs dark" should be
 * solved with Tailwind's `dark:` variant or CSS variables — both
 * resolve from the `.dark` class on `<html>` automatically, with no
 * React re-render and no risk of stale state.
 *
 * Examples that should NOT use this hook:
 *   - Swapping images:
 *       <img src="/logo-light.svg" className="block dark:hidden" />
 *       <img src="/logo-dark.svg"  className="hidden dark:block" />
 *   - Choosing colors: use semantic tokens (`bg-card`, `text-foreground`)
 *     or `dark:` overrides (`bg-purple-100 dark:bg-purple-500`).
 *   - Conditional classNames: `className="border-purple-400 dark:border-purple-500/40"`.
 *
 * Use this hook ONLY when you genuinely need to branch in JS — e.g. a
 * theme-toggle button picking which icon to render, or analytics that
 * report the active theme. Those cases are rare.
 *
 * Returns `[theme, setTheme]`. `setTheme` updates the `.dark` class on
 * `<html>` and persists the choice to localStorage.
 */
export function useTheme(): [Theme, Dispatch<SetStateAction<Theme>>] {
  const [theme, setTheme] = useState<Theme>(getInitialTheme);

  // Keep the DOM + storage in sync whenever the state changes (e.g.
  // user clicked the toggle, or another component called `setTheme`).
  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  return [theme, setTheme];
}

/** Convenience helper for a toggle button. Returns a callback that
 *  flips light ↔ dark in a single click. */
export function useThemeToggle(): { theme: Theme; toggle: () => void } {
  const [theme, setTheme] = useTheme();
  const toggle = useCallback(() => {
    setTheme((prev) => (prev === "dark" ? "light" : "dark"));
  }, [setTheme]);
  return { theme, toggle };
}
