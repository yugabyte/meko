import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import { extname, resolve } from 'node:path';
import { DESIGN_FEEDBACK_ID, type DesignAnswer, type DesignQuestion, type DesignQuestionOption } from '../shared/types.js';

/** A published Artifact's HTML larger than this isn't kept (the claude.ai link still is). */
export const MAX_DESIGN_HTML = 5 * 1024 * 1024;
/** A markdown design file larger than this is ignored. */
export const MAX_DESIGN_MARKDOWN = 1024 * 1024;

/** A questions file larger than this is ignored. */
export const MAX_QUESTIONS_FILE = 256 * 1024;
/** Most questions kept from a design; later ones are dropped. */
export const MAX_QUESTIONS = 30;
/** Most options kept per question. */
export const MAX_OPTIONS = 6;
const QUESTION_CHARS = 1000;
const CONTEXT_CHARS = 4000;
const LABEL_CHARS = 200;
const DETAIL_CHARS = 1000;
const DECISION_CHARS = 2000;
const ID_CHARS = 64;

const ARTIFACT_URL = /https:\/\/claude\.ai\/(?:code\/)?artifact\/[A-Za-z0-9_-]+/;

/** The first claude.ai Artifact link in some text (a tool result, a design file), if any. */
export function artifactUrlIn(text: string): string | null {
  return ARTIFACT_URL.exec(text)?.[0] ?? null;
}

/**
 * The Meko filename for version `version` (from 1) of a session's design. Meko can't replace a
 * file, so each later version gets its own name: design-<slug>.md, design-<slug>-v2.md, …
 */
export function mekoDesignName(slug: string, ext: 'md' | 'html', version: number): string {
  return `design-${slug}${version > 1 ? `-v${version}` : ''}.${ext}`;
}

/** True for an Artifact tool call that publishes a page (not a read, list, asset upload…). */
export function isArtifactPublish(input: Record<string, unknown>): boolean {
  const action = input.action;
  return (action === undefined || action === 'publish') && !input.asset && typeof input.file_path === 'string';
}

/** The HTML page an Artifact was published from, or null when it isn't a readable .html file. */
export function readArtifactHtml(filePath: unknown, cwd: string | undefined): string | null {
  if (typeof filePath !== 'string' || !/^\.html?$/i.test(extname(filePath))) return null;
  return readCapped(resolve(cwd || '/', filePath), MAX_DESIGN_HTML);
}

/** A markdown design file's text, or null when it's missing, empty or too large. */
export function readDesignFile(file: string): string | null {
  const text = readCapped(file, MAX_DESIGN_MARKDOWN);
  return text?.trim() ? text : null;
}

function readCapped(file: string, max: number): string | null {
  try {
    const st = statSync(file);
    if (!st.isFile() || st.size > max) return null;
    return readFileSync(file, 'utf8');
  } catch {
    return null;
  }
}

/** Cut to `max` characters plus '…' when longer. */
function clip(text: string, max: number): string {
  return text.length > max ? `${text.slice(0, max)}…` : text;
}

/** A trimmed, capped string, or null for anything else or an empty one. */
function str(v: unknown, max: number): string | null {
  return typeof v === 'string' && v.trim() ? clip(v.trim(), max) : null;
}

/** A question id as stored: lowercase letters, digits and dashes (other characters become dashes). */
function questionId(v: unknown): string | null {
  if (typeof v !== 'string' && typeof v !== 'number') return null;
  const id = String(v).toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, ID_CHARS).replace(/-+$/, '');
  return id || null;
}

function option(v: unknown): DesignQuestionOption | null {
  if (typeof v === 'string') {
    const label = str(v, LABEL_CHARS);
    return label ? { label, detail: null } : null;
  }
  if (!v || typeof v !== 'object') return null;
  const o = v as Record<string, unknown>;
  const label = str(o.label, LABEL_CHARS);
  return label ? { label, detail: str(o.detail ?? o.description, DETAIL_CHARS) } : null;
}

/**
 * The questions in a questions file's JSON (`{"questions": [...]}` or a bare array), normalized:
 * entries without a question or a usable id, and repeated ids, are dropped, text is capped, and
 * a recommendation must point at an option. Throws when the JSON or its shape is wrong.
 */
export function parseDesignQuestions(text: string): DesignQuestion[] {
  const data: unknown = JSON.parse(text);
  const list = Array.isArray(data) ? data : data && typeof data === 'object' ? (data as Record<string, unknown>).questions : undefined;
  if (!Array.isArray(list)) throw new Error('expected {"questions": [...]}');
  const out: DesignQuestion[] = [];
  const seen = new Set<string>();
  for (const v of list) {
    if (out.length >= MAX_QUESTIONS) break;
    if (!v || typeof v !== 'object') continue;
    const q = v as Record<string, unknown>;
    const id = questionId(q.id);
    const question = str(q.question, QUESTION_CHARS);
    if (!id || !question || seen.has(id)) continue;
    seen.add(id);
    const options = (Array.isArray(q.options) ? q.options : []).map(option).filter((o): o is DesignQuestionOption => !!o).slice(0, MAX_OPTIONS);
    const rec = q.recommended;
    const resolved = typeof q.status === 'string' && /^(resolved|answered|closed|decided)$/i.test(q.status.trim());
    out.push({
      id,
      question,
      context: str(q.context, CONTEXT_CHARS),
      options,
      recommended: Number.isInteger(rec) && (rec as number) >= 0 && (rec as number) < options.length ? (rec as number) : null,
      status: resolved ? 'resolved' : 'open',
      decision: resolved ? str(q.decision, DECISION_CHARS) : null,
    });
  }
  return out;
}

/**
 * The questions file AgentK writes beside the design: null when there's none (or it's empty or
 * too large); throws when it can't be read as questions.
 */
export function readDesignQuestions(file: string): DesignQuestion[] | null {
  const text = readCapped(file, MAX_QUESTIONS_FILE);
  return text?.trim() ? parseDesignQuestions(text) : null;
}

const HEADING = /^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const ITEM = /^ {0,1}(?:[-*+]|\d{1,3}[.)])\s+(.*)$/;
const FENCE = /^ {0,3}(```|~~~)/;

/**
 * For a markdown design without a questions file: the top-level list items under its "Open
 * questions" heading, as open questions without options. Under a heading that also names risks
 * ("Risks and open questions") only the items that ask something (with a "?") count. Ids come
 * from the text, so they stay the same while an item does.
 */
export function questionsFromMarkdown(markdown: string): DesignQuestion[] {
  const lines = markdown.split(/\r?\n/);
  const items: string[] = [];
  let level = 0;
  let onlyAsks = false;
  let inFence = false;
  let current: string | null = null;
  const flush = () => {
    if (current !== null && (!onlyAsks || current.includes('?'))) items.push(current);
    current = null;
  };
  for (const line of lines) {
    if (FENCE.test(line)) inFence = !inFence;
    const h = inFence ? null : HEADING.exec(line);
    if (h) {
      if (level && h[1].length <= level) {
        flush();
        level = 0;
      }
      if (!level && /\bopen questions?\b/i.test(h[2])) {
        level = h[1].length;
        onlyAsks = /\brisks?\b/i.test(h[2]);
      }
      continue;
    }
    if (!level) continue;
    const item = inFence ? null : ITEM.exec(line);
    if (item) {
      flush();
      current = item[1];
    } else if (current !== null && line.trim()) {
      // Lines under an item (wrapped text, nested lists) are part of it, past blank lines too.
      current += ` ${line.trim()}`;
    }
  }
  flush();
  const out: DesignQuestion[] = [];
  const seen = new Set<string>();
  for (const raw of items) {
    const question = clip(raw.replace(/\*\*|__/g, '').replace(/\s+/g, ' ').trim(), QUESTION_CHARS);
    if (!question) continue;
    const id = `md-${createHash('sha256').update(question).digest('hex').slice(0, 8)}`;
    if (seen.has(id)) continue;
    seen.add(id);
    out.push({ id, question, context: null, options: [], recommended: null, status: 'open', decision: null });
    if (out.length >= MAX_QUESTIONS) break;
  }
  return out;
}

/** Longest answer, in characters. */
export const ANSWER_MAX = 10_000;
/** Longest set of answers sent to Claude at once, in characters. */
export const ANSWERS_PROMPT_MAX = 50_000;

/** "2 answers to open questions and other feedback", for the prompt's first line. */
export function answersCount(answers: DesignAnswer[]): string {
  const n = answers.filter((a) => a.questionId !== DESIGN_FEEDBACK_ID).length;
  const parts = [];
  if (n) parts.push(n === 1 ? '1 answer to an open question' : `${n} answers to open questions`);
  if (answers.some((a) => a.questionId === DESIGN_FEEDBACK_ID)) parts.push(n ? 'other feedback' : 'feedback');
  return parts.join(' and ');
}

/**
 * The answers as markdown for Claude: in the order of the design's questions (numbered as the
 * user saw them), then answers to questions no longer in the design, then other feedback.
 */
export function formatAnswers(answers: DesignAnswer[], questions: DesignQuestion[]): string {
  const order = new Map(questions.map((q, i) => [q.id, i]));
  const rank = (a: DesignAnswer) => (a.questionId === DESIGN_FEEDBACK_ID ? 2 : order.has(a.questionId) ? 0 : 1);
  const sorted = [...answers].sort((a, b) => rank(a) - rank(b)
    || (order.get(a.questionId) ?? 0) - (order.get(b.questionId) ?? 0) || a.createdAt - b.createdAt);
  return sorted.map((a) => {
    const text = a.text.trim();
    if (a.questionId === DESIGN_FEEDBACK_ID) return `### Other feedback\n${text}`;
    const i = order.get(a.questionId);
    const head = i === undefined ? `### Earlier question (id: ${a.questionId}): ${a.question}` : `### Q${i + 1} (id: ${a.questionId}): ${a.question}`;
    const lines = [head];
    if (a.choiceLabel !== null) lines.push(`Answer: ${a.choiceLabel}`);
    else if (a.yourCall) lines.push('Answer: your call. Go with your recommendation.');
    if (text) lines.push(a.choiceLabel !== null || a.yourCall ? `Note: ${text}` : `Answer: ${text}`);
    return lines.join('\n');
  }).join('\n\n');
}
