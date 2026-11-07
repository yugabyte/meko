import { useSyncExternalStore } from 'react';

/**
 * The GitHub dialog's opener: any API call refused because gh isn't installed or signed in (code
 * GH_NEEDED, from `api.ts`) opens it with the refusal's words, whichever part of the page made the
 * call. `GhDialog.tsx`, mounted once in the app, reads it.
 */
let reason: string | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  for (const l of listeners) l();
}

export function openGhDialog(why: string): void {
  reason = why;
  emit();
}

export function closeGhDialog(): void {
  reason = null;
  emit();
}

function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

/** Why the GitHub dialog is open, or null while it's closed. */
export function ghDialogReason(): string | null {
  return reason;
}

export function useGhDialog(): string | null {
  return useSyncExternalStore(subscribe, ghDialogReason);
}
