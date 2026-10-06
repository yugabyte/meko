import type { DesignAnswerInput, DesignQuestion } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get } from './db.js';
import { ANSWER_MAX, ANSWERS_PROMPT_MAX, answersCount, formatAnswers, questionsFromMarkdown, readDesignQuestions } from './design.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';
import type { SessionRow } from './store.js';

/** A design's questions for the user, where they came from, and how many are open. */
export interface DesignQuestionSet {
  questions: DesignQuestion[];
  /**
   * `file` (the questions file Claude writes beside the design), `markdown` (the "Open questions"
   * list of a markdown design), or null (none).
   */
  source: 'file' | 'markdown' | null;
  open: number;
}

interface Entry extends DesignQuestionSet {
  json: string;
}

function entry(questions: DesignQuestion[], source: DesignQuestionSet['source']): Entry {
  return { questions, source, open: questions.filter((q) => q.status === 'open').length, json: JSON.stringify(questions) };
}

/**
 * The questions of each session's design, kept in memory: nothing about them is stored in the
 * database. They're read from the questions file Claude writes beside the design (or else the
 * "Open questions" list of a markdown design) the first time they're asked for, again at the end
 * of each Design turn (`refresh`), and for every session in Design at startup (`preload`), so the
 * first turn end after a restart can tell whether they changed.
 */
export class DesignQuestionStore {
  private cache = new Map<string, Entry>();

  constructor(private core: Core) {}

  /** A session's questions; none until it has a design. */
  get(row: SessionRow): DesignQuestionSet {
    let e = this.cache.get(row.id);
    if (!e) {
      try {
        e = this.read(row);
      } catch (err) {
        // A broken questions file with nothing read before it (after a restart): the markdown
        // design's list, if it has one, until Claude fixes it. Said in the activity log, since the
        // questions it had are gone and Approve no longer asks for them.
        this.core.log.warn(`reading the design's questions failed for ${row.id}`, errMsg(err));
        if (row.stage === 'design') {
          this.core.sessions.event(row.id, 'design', `AgentK's questions file can't be read (${errMsg(err)}), so its open questions aren't shown. Ask AgentK to fix ${this.core.sessions.questionsFile(row)}.`);
        }
        e = this.read(row, true);
      }
      this.cache.set(row.id, e);
    }
    return e;
  }

  /**
   * Reads a session's questions again; true when they changed. Throws when the questions file is
   * too large, unreadable or not questions, leaving the ones kept alone.
   */
  refresh(row: SessionRow): boolean {
    const next = this.read(row);
    const prev = this.cache.get(row.id);
    this.cache.set(row.id, next);
    // With nothing kept to compare with, no one has seen the questions change.
    return !!prev && (prev.json !== next.json || prev.source !== next.source);
  }

  /** Loads the questions of every running session in Design. */
  preload(): void {
    for (const row of all<SessionRow>(this.core.db, `SELECT * FROM sessions WHERE desired = 'running' AND stage = 'design'`)) this.get(row);
  }

  forget(id: string): void {
    this.cache.delete(id);
  }

  private read(row: SessionRow, skipFile = false): Entry {
    const d = get<{ markdown: string }>(this.core.db, 'SELECT markdown FROM designs WHERE session_id = ?', row.id);
    if (!d) return entry([], null);
    const fromFile = skipFile ? null : readDesignQuestions(this.core.sessions.questionsFile(row));
    if (fromFile) return entry(fromFile, 'file');
    const fromMd = d.markdown ? questionsFromMarkdown(d.markdown) : [];
    return entry(fromMd, fromMd.length ? 'markdown' : null);
  }
}

/** Refused: the browser's answers are to questions AgentK has changed since. */
const STALE = 'AgentK changed its questions since you answered. Review them and try again.';

/**
 * The user's answers to the design's open questions (and other feedback) from a request body,
 * checked against the current questions and formatted for Claude, with the count for the
 * prompt's first line. Null when there's nothing in it. Throws a 400 for a malformed answer or
 * answers too long for one prompt, and a 409 for an answer to a question that's no longer open
 * or whose options changed.
 */
export function designAnswers(body: unknown, questions: DesignQuestion[]): { text: string; count: string } | null {
  if (body === undefined || body === null) return null;
  if (typeof body !== 'object' || Array.isArray(body)) throw new HttpError(400, 'Answers must be {answers, feedback}.');
  const b = body as Record<string, unknown>;
  if (b.answers !== undefined && !Array.isArray(b.answers)) throw new HttpError(400, 'answers must be a list.');
  if (b.feedback !== undefined && typeof b.feedback !== 'string') throw new HttpError(400, 'feedback must be text.');
  const feedback = (b.feedback as string | undefined) ?? '';
  const capped = (what: string, text: string) => {
    const n = text.trim().length;
    if (n > ANSWER_MAX) {
      throw new HttpError(400, `${what} can be at most ${ANSWER_MAX.toLocaleString('en-US')} characters; it has ${n.toLocaleString('en-US')}.`);
    }
  };
  capped('Other feedback', feedback);

  const number = new Map(questions.map((q, i) => [q.id, i + 1]));
  const answers: DesignAnswerInput[] = [];
  const seen = new Set<string>();
  for (const v of (b.answers as unknown[] | undefined) ?? []) {
    if (!v || typeof v !== 'object') throw new HttpError(400, 'Each answer must be {questionId, choice, label, text}.');
    const a = v as Record<string, unknown>;
    if (typeof a.questionId !== 'string') throw new HttpError(400, 'Each answer needs its questionId.');
    if (seen.has(a.questionId)) throw new HttpError(400, `There are two answers to ${a.questionId}.`);
    seen.add(a.questionId);
    const q = questions.find((x) => x.id === a.questionId);
    if (!q || q.status !== 'open') throw new HttpError(409, STALE);
    const qn = `Q${number.get(q.id)}`;
    const text = a.text ?? '';
    if (typeof text !== 'string') throw new HttpError(400, `The answer to ${qn} must be text.`);
    capped(`The answer to ${qn}`, text);
    const choice = a.choice ?? null;
    if (choice === null) {
      if (!text.trim()) throw new HttpError(400, `Write your answer to ${qn}, or pick one of AgentK's options.`);
      answers.push({ questionId: q.id, choice: null, label: null, text });
      continue;
    }
    if (!Number.isInteger(choice) || (choice as number) < 0) throw new HttpError(400, `Pick one of ${qn}'s options.`);
    if (typeof a.label !== 'string') throw new HttpError(400, `The answer to ${qn} needs the label of the option picked.`);
    // The label must still be the option's: a revision may have moved, changed or dropped it.
    const option = q.options[choice as number];
    if (!option || option.label !== a.label) throw new HttpError(409, STALE);
    answers.push({ questionId: q.id, choice: choice as number, label: option.label, text });
  }
  if (!answers.length && !feedback.trim()) return null;
  const text = formatAnswers(answers, feedback, questions);
  if (text.length > ANSWERS_PROMPT_MAX) {
    throw new HttpError(400, `Your answers come to ${text.length.toLocaleString('en-US')} characters, more than the ${ANSWERS_PROMPT_MAX.toLocaleString('en-US')} that go to AgentK at once. Shorten some.`);
  }
  return { text, count: answersCount(answers, feedback) };
}
