import { useSyncExternalStore } from 'react';
import { DEFAULT_REVIEW, resolveReview, type ReviewOverrides, type ReviewSettings } from '../shared/codereview';
import type { Settings } from '../shared/types';
import { api } from './api';

/**
 * The global local review settings (`[code_review]`), for what "Default (…)" stands for in a
 * project's and a session's review settings: read from GET /api/settings once per page, when a view
 * first needs them, and set again by Settings when it loads or saves. Null until read (or when the
 * read failed: "Default" then shows without its value).
 */
let global: ReviewSettings | null = null;
let asked = false;
const listeners = new Set<() => void>();

function emit() {
  listeners.forEach((f) => f());
}

/** Settings loaded or saved: what "Default" stands for from now on. */
export function setReviewDefaults(s: Pick<Settings, 'codeReviewStrategy' | 'codeReviewRounds' | 'codeReviewDepth'>): void {
  global = { strategy: s.codeReviewStrategy ?? DEFAULT_REVIEW.strategy, rounds: s.codeReviewRounds ?? DEFAULT_REVIEW.rounds, depth: s.codeReviewDepth ?? DEFAULT_REVIEW.depth };
  emit();
}

function subscribe(f: () => void) {
  listeners.add(f);
  if (!asked) {
    asked = true;
    api.settings().then(setReviewDefaults, () => {});
  }
  return () => void listeners.delete(f);
}

/** The global review settings, or null until read. */
export function useReviewDefaults(): ReviewSettings | null {
  return useSyncExternalStore(subscribe, () => global, () => null);
}

/** What a level's "Default" stands for: the global settings under `over` (a project's, for a session), or null until read. */
export function inheritedReview(global: ReviewSettings | null, over: ReviewOverrides = {}): ReviewSettings | null {
  return global ? resolveReview(global, over, {}) : null;
}
