import type { AskOption, AskQuestion } from '../shared/types.js';
import { ANSWER_MAX } from './design.js';
import { HttpError } from './projects.js';

/** Most questions in one AskUserQuestion call (Claude Code allows 4). */
export const MAX_ASK_QUESTIONS = 4;
/** Most options kept per question (Claude Code allows 4; a few more are tolerated). */
const MAX_ASK_OPTIONS = 6;
const QUESTION_CHARS = 1000;
const HEADER_CHARS = 100;
const LABEL_CHARS = 200;
const DESCRIPTION_CHARS = 1000;
const PREVIEW_CHARS = 8000;

function text(v: unknown, max: number, what: string): string {
  if (typeof v !== 'string' || !v.trim()) throw new Error(`${what} must be text`);
  return v.length > max ? `${v.slice(0, max)}…` : v;
}

function optional(v: unknown, max: number): string | null {
  if (typeof v !== 'string' || !v.trim()) return null;
  return v.length > max ? `${v.slice(0, max)}…` : v;
}

/**
 * The questions of a Claude AskUserQuestion call, as the mod forwards them, checked and capped for
 * the browser. Throws for anything AgentK can't show as Claude's dialog does: then the dialog in
 * the terminal is left to answer it alone. Questions of another kind than a choice (Claude Code's
 * `text` and `number` questions) are among those.
 */
export function parseAsk(v: unknown): AskQuestion[] {
  if (!Array.isArray(v) || !v.length || v.length > MAX_ASK_QUESTIONS) throw new Error(`questions must be a list of 1 to ${MAX_ASK_QUESTIONS}`);
  return v.map((raw, i) => {
    if (!raw || typeof raw !== 'object') throw new Error(`question ${i + 1} must be an object`);
    const q = raw as Record<string, unknown>;
    if (q.kind !== undefined && q.kind !== 'choice') throw new Error(`question ${i + 1} isn't a choice`);
    if (!Array.isArray(q.options) || !q.options.length || q.options.length > MAX_ASK_OPTIONS) {
      throw new Error(`question ${i + 1} must have 1 to ${MAX_ASK_OPTIONS} options`);
    }
    const labels = new Set<string>();
    const options = q.options.map((o, j): AskOption => {
      if (!o || typeof o !== 'object') throw new Error(`option ${j + 1} of question ${i + 1} must be an object`);
      const r = o as Record<string, unknown>;
      // Labels are what the answer names, so they're kept whole.
      if (typeof r.label !== 'string' || !r.label.trim() || r.label.length > LABEL_CHARS) {
        throw new Error(`option ${j + 1} of question ${i + 1} needs a label of at most ${LABEL_CHARS} characters`);
      }
      if (labels.has(r.label)) throw new Error(`question ${i + 1} has two options labelled ${r.label}`);
      labels.add(r.label);
      return { label: r.label, description: optional(r.description, DESCRIPTION_CHARS), preview: optional(r.preview, PREVIEW_CHARS) };
    });
    return {
      question: text(q.question, QUESTION_CHARS, `question ${i + 1}`),
      header: optional(q.header, HEADER_CHARS) ?? '',
      options,
      multiSelect: q.multiSelect === true,
    };
  });
}

/**
 * The user's answers to an ask, from a request body (an AskAnswerRequest), checked against its
 * questions: one per question at most, labels that are the question's options (one unless it's
 * multi-select), text within the answer limit. Returns the answer text per question index, as
 * Claude's dialog gives it (the labels comma-joined, then the custom text); a question without
 * an answer is left out. Throws a 400 for anything else.
 */
export function askAnswers(body: unknown, questions: AskQuestion[]): Record<number, string> {
  if (!body || typeof body !== 'object' || !Array.isArray((body as Record<string, unknown>).answers)) {
    throw new HttpError(400, 'Answers must be {answers: [...]}.');
  }
  const out: Record<number, string> = {};
  for (const v of (body as { answers: unknown[] }).answers) {
    if (!v || typeof v !== 'object') throw new HttpError(400, 'Each answer must be {question, labels, text}.');
    const a = v as Record<string, unknown>;
    const n = a.question;
    if (!Number.isInteger(n) || (n as number) < 0 || (n as number) >= questions.length) throw new HttpError(400, 'An answer names a question AgentK isn\'t asking.');
    const q = questions[n as number];
    const qn = `question ${(n as number) + 1}`;
    if ((n as number) in out) throw new HttpError(400, `There are two answers to ${qn}.`);
    const labels = a.labels ?? [];
    if (!Array.isArray(labels) || labels.some((l) => typeof l !== 'string')) throw new HttpError(400, `The options picked for ${qn} must be a list of labels.`);
    if (new Set(labels).size !== labels.length) throw new HttpError(400, `An option is picked twice for ${qn}.`);
    if (labels.length > 1 && !q.multiSelect) throw new HttpError(400, `Pick one option for ${qn}.`);
    for (const l of labels as string[]) {
      if (!q.options.some((o) => o.label === l)) throw new HttpError(400, `${l} isn't one of the options of ${qn}.`);
    }
    const t = a.text ?? '';
    if (typeof t !== 'string') throw new HttpError(400, `The answer to ${qn} must be text.`);
    if (t.trim().length > ANSWER_MAX) throw new HttpError(400, `The answer to ${qn} can be at most ${ANSWER_MAX.toLocaleString('en-US')} characters.`);
    const parts = [...(labels as string[]), ...(t.trim() ? [t.trim()] : [])];
    if (!parts.length) throw new HttpError(400, `Pick an option for ${qn} or write an answer.`);
    out[n as number] = parts.join(', ');
  }
  if (!Object.keys(out).length) throw new HttpError(400, 'Answer at least one question.');
  return out;
}
