import { randomUUID } from 'node:crypto';
import { DESIGN_FEEDBACK_ID, type CommentState, type DesignAnswer, type DesignQuestion, type SendCommentsResult } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, run, tx, type Db } from './db.js';
import { ANSWER_MAX, ANSWERS_PROMPT_MAX, answersCount, formatAnswers } from './design.js';
import { DESIGN_READY } from './lifecycle.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';
import type { SessionRow } from './store.js';

interface AnswerRow {
  id: string;
  session_id: string;
  question_id: string;
  question: string;
  choice: number | null;
  choice_label: string | null;
  your_call: number;
  text: string;
  state: CommentState;
  created_at: number;
  updated_at: number;
  sent_at: number | null;
}

function toAnswer(r: AnswerRow): DesignAnswer {
  return {
    id: r.id, questionId: r.question_id, question: r.question, choice: r.choice, choiceLabel: r.choice_label,
    yourCall: !!r.your_call, text: r.text, state: r.state, createdAt: r.created_at, updatedAt: r.updated_at, sentAt: r.sent_at,
  };
}

/** The questions stored with a session's design, in AgentK's order; [] without a design. */
export function designQuestions(db: Db, sessionId: string): DesignQuestion[] {
  const r = get<{ questions: string }>(db, 'SELECT questions FROM designs WHERE session_id = ?', sessionId);
  try {
    const q: unknown = JSON.parse(r?.questions ?? '[]');
    return Array.isArray(q) ? (q as DesignQuestion[]) : [];
  } catch {
    return [];
  }
}

/**
 * The design's open questions, the answers not yet sent to Claude, and when any answer last
 * changed, for `Session.designQuestions`.
 */
export function designQuestionCounts(db: Db, sessionId: string): { open: number; unsent: number; changedAt: number | null } {
  const open = get<{ n: number }>(db, 'SELECT open_questions AS n FROM designs WHERE session_id = ?', sessionId)?.n ?? 0;
  const a = get<{ unsent: number; at: number | null }>(db,
    `SELECT COALESCE(SUM(state != 'sent'), 0) AS unsent, MAX(updated_at) AS at FROM design_answers WHERE session_id = ?`, sessionId);
  return { open, unsent: a?.unsent ?? 0, changedAt: a?.at ?? null };
}

/**
 * The user's answers to a design's open questions, and other feedback on it. Drafts are kept
 * until the user sends them; sent answers are pasted into Claude as one prompt when it's at its
 * prompt (a design waiting for approval included), or queued until its turn ends
 * (Lifecycle.onTurnEnd, and the periodic poll after a restart). Approving the design takes the
 * answers not yet sent into the approval prompt (`forApproval`).
 */
export class DesignAnswerService {
  private delivering = new Set<string>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  /** A running session in the Design stage, where answers can change and go out. */
  private designing(sessionId: string): SessionRow {
    const row = this.core.sessions.row(sessionId);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (row.stage !== 'design' || row.plan_mode) throw new HttpError(409, 'Answers to the design can only be sent while the session is in Design.');
    return row;
  }

  private rows(sessionId: string, where: string): AnswerRow[] {
    return all<AnswerRow>(this.db, `SELECT * FROM design_answers WHERE session_id = ? AND ${where} ORDER BY created_at, id`, sessionId);
  }

  list(sessionId: string): DesignAnswer[] {
    this.core.sessions.row(sessionId);
    return this.rows(sessionId, '1').map(toAnswer);
  }

  /**
   * Changes the draft answer to one question (or the other feedback): fields left out keep their
   * value, an option clears Your call and the other way round, and an empty answer is removed.
   */
  put(sessionId: string, questionId: string, body: unknown): DesignAnswer[] {
    this.designing(sessionId);
    const b = (body && typeof body === 'object' ? body : {}) as Record<string, unknown>;
    const feedback = questionId === DESIGN_FEEDBACK_ID;
    const q = feedback ? null : designQuestions(this.db, sessionId).find((x) => x.id === questionId);
    if (!feedback && !q) throw new HttpError(400, 'That question isn’t in the design. Reload it to see the current questions.');
    if (q && q.status !== 'open') throw new HttpError(409, 'That question is no longer open: AgentK resolved it.');
    if (b.choice !== undefined && b.choice !== null && (!Number.isInteger(b.choice) || !q || (b.choice as number) < 0 || (b.choice as number) >= q.options.length)) {
      throw new HttpError(400, 'Pick one of the question’s options.');
    }
    if (b.yourCall !== undefined && typeof b.yourCall !== 'boolean') throw new HttpError(400, 'yourCall must be true or false.');
    if (b.text !== undefined && typeof b.text !== 'string') throw new HttpError(400, 'The answer must be text.');
    if (typeof b.text === 'string' && b.text.trim().length > ANSWER_MAX) {
      throw new HttpError(400, `Answers can be at most ${ANSWER_MAX.toLocaleString('en-US')} characters; this one has ${b.text.trim().length.toLocaleString('en-US')}.`);
    }
    if (feedback && (b.choice != null || b.yourCall)) throw new HttpError(400, 'Other feedback is text only.');

    const unsent = get<AnswerRow>(this.db, `SELECT * FROM design_answers WHERE session_id = ? AND question_id = ? AND state != 'sent'`, sessionId, questionId);
    if (unsent?.state === 'queued') throw new HttpError(409, 'Your answer to this question is waiting to be sent to AgentK, so it can no longer be changed.');
    // The option is kept by its label too: what the user picked, even after a revision changes the
    // options (see realign). An index is only used here once checked against the current options.
    let choice = unsent?.choice ?? null;
    let label = unsent?.choice_label ?? null;
    let yourCall = !!unsent?.your_call;
    if (b.choice !== undefined) {
      choice = b.choice as number | null;
      label = choice === null ? null : q!.options[choice].label;
      if (choice !== null) yourCall = false;
    }
    if (b.yourCall !== undefined) {
      yourCall = b.yourCall as boolean;
      if (yourCall) {
        choice = null;
        label = null;
      }
    }
    const text = typeof b.text === 'string' ? b.text : unsent?.text ?? '';
    const now = Date.now();
    if (label === null && !yourCall && !text.trim()) {
      if (unsent) run(this.db, 'DELETE FROM design_answers WHERE id = ?', unsent.id);
    } else {
      const question = q?.question ?? 'Other feedback';
      if (unsent) {
        run(this.db, 'UPDATE design_answers SET question = ?, choice = ?, choice_label = ?, your_call = ?, text = ?, updated_at = ? WHERE id = ?',
          question, choice, label, yourCall ? 1 : 0, text, now, unsent.id);
      } else {
        run(this.db, `INSERT INTO design_answers(id, session_id, question_id, question, choice, choice_label, your_call, text, state, created_at, updated_at)
                      VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?)`,
          randomUUID(), sessionId, questionId, question, choice, label, yourCall ? 1 : 0, text, now, now);
      }
    }
    this.core.changed();
    return this.list(sessionId);
  }

  /** Drops the draft answer to a question, whatever its question's state. */
  remove(sessionId: string, questionId: string): void {
    const row = this.core.sessions.row(sessionId);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    const n = run(this.db, `DELETE FROM design_answers WHERE session_id = ? AND question_id = ? AND state = 'draft'`, sessionId, questionId).changes;
    if (!n) throw new HttpError(404, 'There’s no draft answer to that question.');
    this.core.changed();
  }

  /** Queues every draft, then pastes the queue into Claude if it's at its prompt. */
  async send(sessionId: string): Promise<SendCommentsResult> {
    const row = this.designing(sessionId);
    if (row.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session to send your answers.');
    const pending = this.rows(sessionId, `state != 'sent'`).map(toAnswer);
    if (!pending.length) throw new HttpError(409, 'There are no answers to send.');
    this.checkSize(formatAnswers(pending, designQuestions(this.db, sessionId)).length);
    run(this.db, `UPDATE design_answers SET state = 'queued', updated_at = ? WHERE session_id = ? AND state = 'draft'`, Date.now(), sessionId);
    this.core.changed();
    let sent: number;
    try {
      sent = await this.deliver(sessionId);
    } catch (e) {
      throw new HttpError(502, `Couldn't paste your answers into Claude's terminal: ${errMsg(e)}. They stay queued and are sent when AgentK finishes its turn.`);
    }
    return { sent, queued: this.rows(sessionId, `state = 'queued'`).length };
  }

  /** How many answers wait for Claude's turn to end. */
  queued(sessionId: string): number {
    return this.rows(sessionId, `state = 'queued'`).length;
  }

  /**
   * Pastes queued answers into Claude when it's in Design and at its prompt: between turns, or
   * with a design waiting for approval (never while it shows a permission or question prompt).
   * Returns how many went out.
   */
  async deliver(sessionId: string): Promise<number> {
    if (this.delivering.has(sessionId)) return 0;
    const row = this.core.sessions.row(sessionId);
    const atPrompt = row.status === 'idle' || row.status === 'recovered' || (row.status === 'needs_input' && row.status_detail === DESIGN_READY);
    if (row.desired !== 'running' || row.stage !== 'design' || row.plan_mode || !atPrompt || this.core.sessions.runtime(sessionId).busy) return 0;
    const queued = this.rows(sessionId, `state = 'queued'`).map(toAnswer);
    if (!queued.length) return 0;
    this.delivering.add(sessionId);
    try {
      const answers = formatAnswers(queued, designQuestions(this.db, sessionId));
      await this.core.sessions.submit(sessionId, this.core.sessions.prompt(row, 'design-answers', { count: answersCount(queued), answers }));
      this.markSent(queued);
      this.core.sessions.event(sessionId, 'design', `Sent ${answersCount(queued)} to AgentK`);
      return queued.length;
    } finally {
      this.delivering.delete(sessionId);
    }
  }

  /**
   * For approving the design: the answers not yet sent, formatted for the approval prompt, and
   * `sent()` to mark them sent once it's typed. Null when there are none; throws when they're too
   * long for one prompt, as send does.
   */
  forApproval(sessionId: string): { text: string; count: number; sent: () => void } | null {
    const pending = this.rows(sessionId, `state != 'sent'`).map(toAnswer);
    if (!pending.length) return null;
    const text = formatAnswers(pending, designQuestions(this.db, sessionId));
    this.checkSize(text.length);
    return { text, count: pending.length, sent: () => this.markSent(pending) };
  }

  private checkSize(size: number): void {
    if (size > ANSWERS_PROMPT_MAX) {
      throw new HttpError(400, `Your answers come to ${size.toLocaleString('en-US')} characters, more than the ${ANSWERS_PROMPT_MAX.toLocaleString('en-US')} that go to AgentK at once. Shorten some, or send part of them first.`);
    }
  }

  /**
   * After a revision changed the design's questions: each unsent answer's option is found again
   * by its label, since the options may have moved, gone or been replaced. One that's gone keeps
   * its label, which still goes to Claude, with no option picked, so nothing is picked for the user.
   */
  realign(sessionId: string, questions: DesignQuestion[]): void {
    const byId = new Map(questions.map((q) => [q.id, q]));
    let changed = false;
    for (const a of this.rows(sessionId, `state != 'sent' AND choice_label IS NOT NULL`)) {
      const at = byId.get(a.question_id)?.options.findIndex((o) => o.label === a.choice_label) ?? -1;
      const choice = at >= 0 ? at : null;
      if (choice === a.choice) continue;
      run(this.db, 'UPDATE design_answers SET choice = ?, updated_at = ? WHERE id = ?', choice, Date.now(), a.id);
      changed = true;
    }
    if (changed) this.core.changed();
  }

  /** Leaving Design: answers waiting for Claude's turn go back to drafts, sent again only from Design. */
  unqueue(sessionId: string): void {
    if (run(this.db, `UPDATE design_answers SET state = 'draft', updated_at = ? WHERE session_id = ? AND state = 'queued'`, Date.now(), sessionId).changes) {
      this.core.changed();
    }
  }

  private markSent(answers: DesignAnswer[]): void {
    const now = Date.now();
    tx(this.db, () => {
      for (const a of answers) run(this.db, `UPDATE design_answers SET state = 'sent', sent_at = ?, updated_at = ? WHERE id = ?`, now, now, a.id);
    });
    this.core.changed();
  }
}
