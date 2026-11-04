// A design's Key Design Decisions (KDDs, `DesignQuestion`): the questions AgentK asks and the
// decisions it made itself. Shared by the server (the answers it checks and sends to the agent) and the web UI
// (the Questions tab), so both number them the same way.
import type { DesignQuestion } from './types.js';

/** The class of the marker AgentK writes after a KDD's title in the design when it rests on Meko. */
export const MEKO_SOURCE_CLASS = 'meko-source';

/**
 * The marker itself, the same in markdown and an Artifact's HTML. The design panel draws it as
 * Meko's "m"; anywhere else (claude.ai, GitHub, Meko) it reads as plain "from Meko".
 */
export const MEKO_SOURCE_MARK = `<span class="${MEKO_SOURCE_CLASS}">from Meko</span>`;

/**
 * Each entry's number as the user and the agent see it, by id: questions Q1, Q2… and AgentK's own
 * decisions D1, D2…, each counted in the order AgentK listed them.
 */
export function decisionNumbers(questions: DesignQuestion[]): Map<string, string> {
  let q = 0;
  let d = 0;
  return new Map(questions.map((x) => [x.id, x.kind === 'decision' ? `D${++d}` : `Q${++q}`]));
}

/** Whether the user can send an answer to it: an open question, or a decision AgentK made (to change it). */
export function answerable(q: DesignQuestion): boolean {
  return q.kind === 'decision' || q.status === 'open';
}
