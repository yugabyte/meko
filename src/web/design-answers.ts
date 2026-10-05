// Pure helpers for the design's questions and the user's answers (DesignQuestions.tsx).
import { DESIGN_FEEDBACK_ID, type DesignAnswer } from '../shared/types';

export const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/** Per question: the answer still the user's (a draft, or queued), and the last one sent. */
export function answersByQuestion(answers: DesignAnswer[]): { unsent: Map<string, DesignAnswer>; sent: Map<string, DesignAnswer> } {
  const unsent = new Map<string, DesignAnswer>();
  const sent = new Map<string, DesignAnswer>();
  for (const a of answers) {
    if (a.state !== 'sent') unsent.set(a.questionId, a);
    else if ((sent.get(a.questionId)?.sentAt ?? -1) <= (a.sentAt ?? 0)) sent.set(a.questionId, a);
  }
  return { unsent, sent };
}

/** An answer in a few words: the option picked, Your call, or what the user wrote. */
export function answerSummary(a: DesignAnswer): string {
  const text = a.text.trim();
  const head = a.choiceLabel ?? (a.yourCall ? 'Your call' : null);
  if (!head) return text;
  return text ? `${head} · ${text}` : head;
}

/** The send button's label: the answers, or the feedback when that's all there is. */
export function sendLabel(drafts: DesignAnswer[]): string {
  const n = drafts.filter((a) => a.questionId !== DESIGN_FEEDBACK_ID).length;
  if (n) return `Send ${plural(n, 'answer')}`;
  return 'Send feedback';
}
