// The user's answers to a design's open questions (DesignQuestions.tsx). They aren't stored on the
// server: this browser keeps them, per session, until they go to AgentK with an approval or a
// request to revise the design.
import { useCallback, useEffect, useState } from 'react';
import type { DesignAnswerInput, DesignAnswersRequest, DesignQuestion, Session } from '../shared/types';
import { api } from './api';

export const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/**
 * One question's answer: an option (by index, and by label so it can be found again after a
 * revision moves it) or a custom answer, and its text: a note on the option, or the custom answer.
 */
export interface Draft {
  choice: number | null;
  label: string | null;
  custom: boolean;
  text: string;
}

export interface Drafts {
  answers: Record<string, Draft>;
  feedback: string;
  /**
   * An approval with these answers waits for AgentK's turn to end (`Session.approvalQueued`): they
   * go once it's sent, and are kept if it's dropped.
   */
  approved?: boolean;
}

const EMPTY: Drafts = { answers: {}, feedback: '' };
const key = (sessionId: string) => `ak-design-answers:${sessionId}`;

// Drafts for a page that can't use localStorage (or stopped being able to: a full quota). Where it
// works, localStorage is read every time, so another tab's changes are never missed.
const memory = new Map<string, Drafts>();
const listeners = new Set<(sessionId: string) => void>();

function isDraft(v: unknown): v is Draft {
  if (!v || typeof v !== 'object') return false;
  const d = v as Record<string, unknown>;
  return (d.choice === null || Number.isInteger(d.choice)) && (d.label === null || typeof d.label === 'string')
    && typeof d.custom === 'boolean' && typeof d.text === 'string';
}

/** Drafts from stored JSON, dropping anything that isn't one. */
export function parseDrafts(text: string | null): Drafts {
  if (!text) return EMPTY;
  try {
    const v = JSON.parse(text) as Record<string, unknown>;
    const answers: Record<string, Draft> = {};
    if (v.answers && typeof v.answers === 'object') {
      for (const [id, d] of Object.entries(v.answers as Record<string, unknown>)) if (isDraft(d)) answers[id] = d;
    }
    return { answers, feedback: typeof v.feedback === 'string' ? v.feedback : '', ...(v.approved === true ? { approved: true } : {}) };
  } catch {
    return EMPTY;
  }
}

let persists: boolean | null = null;
/**
 * Whether drafts outlast the page (localStorage works here, and no save has failed), so their
 * fields don't hold up a reload.
 */
export function draftsPersist(): boolean {
  if (persists === null) {
    try {
      localStorage.setItem('ak-design-answers:probe', '1');
      localStorage.removeItem('ak-design-answers:probe');
      persists = true;
    } catch {
      persists = false;
    }
  }
  return persists;
}

function readStorage(sessionId: string): Drafts {
  try {
    return parseDrafts(localStorage.getItem(key(sessionId)));
  } catch {
    return EMPTY;
  }
}

export function loadDrafts(sessionId: string): Drafts {
  if (draftsPersist()) return readStorage(sessionId);
  return memory.get(sessionId) ?? readStorage(sessionId);
}

export function saveDrafts(sessionId: string, d: Drafts): void {
  memory.set(sessionId, d);
  try {
    if (!Object.keys(d.answers).length && !d.feedback) localStorage.removeItem(key(sessionId));
    else localStorage.setItem(key(sessionId), JSON.stringify(d));
  } catch {
    // Kept in memory, for this page only: the fields stop counting as reload-safe.
    persists = false;
  }
  for (const fn of listeners) fn(sessionId);
}

/** Forgets a session's drafts, once they've gone to AgentK. */
export function clearDrafts(sessionId: string): void {
  saveDrafts(sessionId, EMPTY);
}

/** A session's drafts, kept in step with other tabs, and a way to change them. */
export function useDesignDrafts(sessionId: string): [Drafts, (fn: (d: Drafts) => Drafts) => void] {
  const [drafts, setDrafts] = useState(() => loadDrafts(sessionId));
  useEffect(() => {
    setDrafts(loadDrafts(sessionId));
    const onChange = (id: string) => {
      if (id === sessionId) setDrafts(loadDrafts(sessionId));
    };
    // Another tab (the full-page design, or the side panel) changed them.
    const onStorage = (e: StorageEvent) => {
      if (e.key === null || e.key === key(sessionId)) setDrafts(loadDrafts(sessionId));
    };
    listeners.add(onChange);
    window.addEventListener('storage', onStorage);
    return () => {
      listeners.delete(onChange);
      window.removeEventListener('storage', onStorage);
    };
  }, [sessionId]);
  const update = useCallback((fn: (d: Drafts) => Drafts) => saveDrafts(sessionId, fn(loadDrafts(sessionId))), [sessionId]);
  return [drafts, update];
}

/** The answer a question starts with: AgentK's recommendation, nothing, or (without options) the user's own words. */
export function defaultDraft(q: DesignQuestion): Draft {
  if (q.recommended !== null && q.options[q.recommended]) {
    return { choice: q.recommended, label: q.options[q.recommended].label, custom: false, text: '' };
  }
  return { choice: null, label: null, custom: !q.options.length, text: '' };
}

/**
 * A question's answer as shown: the draft, with its option found again by label (a revision may
 * have moved it), or the default when there's none. A draft whose option is gone starts over,
 * keeping its text.
 */
export function draftFor(q: DesignQuestion, d: Draft | undefined): Draft {
  if (!d) return defaultDraft(q);
  if (!q.options.length) return { choice: null, label: null, custom: true, text: d.text };
  if (d.custom) return { ...d, choice: null, label: null };
  if (d.label !== null) {
    const i = q.options.findIndex((o) => o.label === d.label);
    if (i >= 0) return { ...d, choice: i };
  }
  return { ...defaultDraft(q), text: d.text };
}

/**
 * A question's answer as shown, `answerable` or not (past Design, the session archived): there,
 * a question with no answer from this browser shows nothing picked, never the recommendation,
 * which would look like the user's answer.
 */
export function shownDraft(q: DesignQuestion, d: Draft | undefined, answerable: boolean): Draft {
  return answerable || d || !q.options.length ? draftFor(q, d) : { choice: null, label: null, custom: false, text: '' };
}

/**
 * The drafts without the answers to questions that are no longer open (AgentK Local resolved
 * them as they were sent, or AgentK did), or null when there are none to drop.
 */
export function pruneDrafts(questions: DesignQuestion[], drafts: Drafts): Drafts | null {
  const open = new Set(questions.filter((q) => q.status === 'open').map((q) => q.id));
  const ids = Object.keys(drafts.answers);
  if (ids.every((id) => open.has(id))) return null;
  return { ...drafts, answers: Object.fromEntries(ids.filter((id) => open.has(id)).map((id) => [id, drafts.answers[id]])) };
}

/**
 * After answers went to AgentK to revise the design: the feedback is forgotten, and the answers
 * are kept until their questions are no longer open (`pruneDrafts`), so they show until the
 * questions come back resolved, and stay for questions that aren't (a markdown design's).
 */
export function revisionSent(sessionId: string): void {
  saveDrafts(sessionId, { answers: loadDrafts(sessionId).answers, feedback: '' });
}

/**
 * The request for the open questions: the option picked (with any note) or the custom answer.
 * A question with nothing picked, or a custom answer left empty, is left out (unanswered).
 */
export function toRequest(questions: DesignQuestion[], drafts: Drafts): DesignAnswersRequest {
  const answers = questions.flatMap((q): DesignAnswerInput[] => {
    if (q.status !== 'open') return [];
    const d = draftFor(q, drafts.answers[q.id]);
    if (d.custom) return d.text.trim() ? [{ questionId: q.id, choice: null, label: null, text: d.text }] : [];
    return d.choice !== null && d.label !== null ? [{ questionId: q.id, choice: d.choice, label: d.label, text: d.text }] : [];
  });
  return { answers, feedback: drafts.feedback };
}

/**
 * Why the answers can't go yet: Custom answer picked on a question with options, and nothing
 * written. Null when they can. Questions are numbered as the user sees them.
 */
export function draftsBlocker(questions: DesignQuestion[], drafts: Drafts): string | null {
  const i = questions.findIndex((q) => {
    if (q.status !== 'open' || !q.options.length) return false;
    const d = draftFor(q, drafts.answers[q.id]);
    return d.custom && !d.text.trim();
  });
  return i < 0 ? null : `Write your answer to Q${i + 1}, or pick one of AgentK’s options.`;
}

/**
 * After an approval went to the server: the drafts are forgotten, or, when the approval waits for
 * AgentK's turn to end, marked to be forgotten once it's sent (`settleApprovals`).
 */
export function approvalSent(s: Session): void {
  if (s.approvalQueued) saveDrafts(s.id, { ...loadDrafts(s.id), approved: true });
  else clearDrafts(s.id);
}

/**
 * For every session on a state push: drafts whose approval waited for the turn's end are forgotten
 * once it was sent (the session left Design), and kept as drafts again when it was dropped.
 */
export function settleApprovals(sessions: Session[]): void {
  for (const s of sessions) {
    if (s.approvalQueued) continue;
    const d = loadDrafts(s.id);
    if (!d.approved) continue;
    if (s.stage === 'design') saveDrafts(s.id, { answers: d.answers, feedback: d.feedback });
    else clearDrafts(s.id);
  }
}

/** Approves a design with no open questions, taking along any other feedback the user wrote. */
export async function approveDesign(sessionId: string): Promise<void> {
  approvalSent(await api.approveDesign(sessionId, toRequest([], loadDrafts(sessionId))));
}
