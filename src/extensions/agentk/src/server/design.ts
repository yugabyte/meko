import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import { extname, resolve } from 'node:path';
import { decisionNumbers } from '../shared/decisions.js';
import type { DesignAnswerInput, DesignQuestion, DesignQuestionOption, MekoRef } from '../shared/types.js';

/** A published Artifact's HTML larger than this isn't kept (the claude.ai link still is). */
export const MAX_DESIGN_HTML = 5 * 1024 * 1024;
/** A markdown design file larger than this is ignored. */
export const MAX_DESIGN_MARKDOWN = 1024 * 1024;

/** A questions file larger than this is ignored. */
export const MAX_QUESTIONS_FILE = 256 * 1024;
/** Most questions kept from a design; later ones are dropped. */
export const MAX_QUESTIONS = 30;
/** Most of Kode's own decisions kept from a design, counted apart from the questions. */
export const MAX_DECISIONS = 30;
/** Most options kept per question. */
export const MAX_OPTIONS = 6;
/** Most Meko references kept per question or decision. */
export const MAX_MEKO_REFS = 5;
const EXCERPT_CHARS = 500;
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

/** The SHA-256 of some text's UTF-8 bytes, in hex: what Meko names an artifact by. */
export function sha256Hex(text: string): string {
  return createHash('sha256').update(text, 'utf8').digest('hex');
}

/** A design in a PR: its first version's Meko name (which never changes) and the latest version's hash. */
export interface PrDesignRef {
  name: string;
  sha256: string;
}

const DESIGN_LINE = /^\s*meko_design:(design-[A-Za-z0-9._-]+\.(?:md|html));([0-9a-f]{64})\s*$/i;

/**
 * The line that names a session's design in its PR's description: `meko_design:<name>;<sha256>`,
 * the name being the first version's (design-<slug>.md, or .html for an Artifact), so it stays the
 * same while the hash moves to each new version. Meko fetches an artifact only by its hash.
 */
export function designLine(ref: PrDesignRef): string {
  return `meko_design:${ref.name};${ref.sha256}`;
}

/** The design a PR description's `meko_design:` line names, or null when it has none (or a bad one). */
export function parseDesignLine(body: string): PrDesignRef | null {
  for (const line of body.split('\n')) {
    const m = DESIGN_LINE.exec(line);
    if (m) return { name: m[1], sha256: m[2].toLowerCase() };
  }
  return null;
}

/**
 * True for a well-formed `meko_design:<name>;<hash>` line, the only kind Kode writes: a mention
 * of the format in prose or a code block (`meko_design:<name>;…`) isn't one.
 */
export function isDesignLine(line: string): boolean {
  return DESIGN_LINE.test(line);
}

/**
 * A PR description with its `meko_design:` line set to `line`: the first well-formed one replaced
 * (any more dropped), or, without one, the line added after the Meko datapack line
 * (`isDatapackLine`), or at the end. Everything else stays as it was, line endings included.
 */
export function withDesignLine(body: string, line: string, isDatapackLine: (l: string) => boolean): string {
  const lines = body.split('\n');
  const cr = (l: string) => (l.endsWith('\r') ? '\r' : '');
  let replaced = false;
  const out: string[] = [];
  for (const l of lines) {
    if (!isDesignLine(l)) out.push(l);
    else if (!replaced) {
      out.push(line + cr(l));
      replaced = true;
    }
  }
  if (replaced) return out.join('\n');
  const dp = lines.findIndex((l) => isDatapackLine(l.replace(/\r$/, '')));
  if (dp >= 0) {
    lines.splice(dp + 1, 0, line + cr(lines[dp]));
    return lines.join('\n');
  }
  const trimmed = body.replace(/\s+$/, '');
  return trimmed ? `${trimmed}\n\n${line}` : line;
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

/** A Meko id as Kode cites it (a memory's UUID, a knowledge item's id): letters, digits, dashes and underscores. */
const MEKO_ID = /^[A-Za-z0-9_-]{1,100}$/;

/** A decision's kind as written: anything but "decision" is a question. */
function kindOf(v: unknown): DesignQuestion['kind'] {
  return typeof v === 'string' && v.trim().toLowerCase() === 'decision' ? 'decision' : 'question';
}

/** Who decided a question: the user, or Kode (`agentk` in files written before it was called Kode). */
function decidedByOf(v: unknown): DesignQuestion['decidedBy'] {
  if (v === 'agentk') return 'kode';
  return v === 'user' || v === 'kode' ? v : null;
}

/** A Meko reference, or null for one without a kind Kode knows or a usable id. */
function mekoRef(v: unknown): MekoRef | null {
  if (!v || typeof v !== 'object') return null;
  const r = v as Record<string, unknown>;
  const kind = r.kind === 'memory' ? 'memory' : r.kind === 'knowledge' || r.kind === 'knowledge_base' ? 'knowledge' : null;
  const id = typeof r.id === 'string' ? r.id.trim() : '';
  if (!kind || !MEKO_ID.test(id)) return null;
  const dp = r.datapackId ?? r.datapack_id;
  return { kind, id, datapackId: typeof dp === 'string' && MEKO_ID.test(dp.trim()) ? dp.trim() : null, excerpt: str(r.excerpt, EXCERPT_CHARS) };
}

/** The Meko references of an entry: the first MAX_MEKO_REFS that can be read, without repeats. */
function mekoRefs(v: unknown): MekoRef[] {
  const out: MekoRef[] = [];
  for (const r of Array.isArray(v) ? v.map(mekoRef) : []) {
    if (r && !out.some((x) => x.kind === r.kind && x.id === r.id)) out.push(r);
    if (out.length >= MAX_MEKO_REFS) break;
  }
  return out;
}

/**
 * The questions in a questions file's JSON (`{"questions": [...]}` or a bare array), normalized:
 * entries without a question or a usable id, and repeated ids, are dropped, text is capped, and
 * a recommendation must point at an option. A decision (`kind: "decision"`) is always resolved,
 * and one without its decision is dropped. A resolved entry that doesn't say who decided it keeps
 * what `prev` (the questions kept before) said: Kode rewriting the file after the server
 * recorded the user's answers mustn't make them Kode's, or nobody's. Throws when the JSON or its
 * shape is wrong.
 */
export function parseDesignQuestions(text: string, prev: DesignQuestion[] = []): DesignQuestion[] {
  const before = new Map(prev.filter((q) => q.status === 'resolved' && q.decidedBy).map((q) => [q.id, q.decidedBy]));
  const data: unknown = JSON.parse(text);
  const list = Array.isArray(data) ? data : data && typeof data === 'object' ? (data as Record<string, unknown>).questions : undefined;
  if (!Array.isArray(list)) throw new Error('expected {"questions": [...]}');
  const out: DesignQuestion[] = [];
  const seen = new Set<string>();
  const counts = { question: 0, decision: 0 };
  for (const v of list) {
    if (!v || typeof v !== 'object') continue;
    const q = v as Record<string, unknown>;
    const kind = kindOf(q.kind);
    if (counts[kind] >= (kind === 'decision' ? MAX_DECISIONS : MAX_QUESTIONS)) continue;
    const id = questionId(q.id);
    const question = str(q.question, QUESTION_CHARS);
    if (!id || !question || seen.has(id)) continue;
    const resolved = kind === 'decision' || (typeof q.status === 'string' && /^(resolved|answered|closed|decided)$/i.test(q.status.trim()));
    const decision = resolved ? str(q.decision, DECISION_CHARS) : null;
    if (kind === 'decision' && !decision) continue;
    seen.add(id);
    counts[kind]++;
    const options = (Array.isArray(q.options) ? q.options : []).map(option).filter((o): o is DesignQuestionOption => !!o).slice(0, MAX_OPTIONS);
    const rec = q.recommended;
    out.push({
      id,
      kind,
      question,
      context: str(q.context, CONTEXT_CHARS),
      options,
      recommended: Number.isInteger(rec) && (rec as number) >= 0 && (rec as number) < options.length ? (rec as number) : null,
      status: resolved ? 'resolved' : 'open',
      decision,
      // Kode's own decision is its until the user changes it.
      decidedBy: resolved ? (decidedByOf(q.decidedBy) ?? before.get(id) ?? (kind === 'decision' ? 'kode' : null)) : null,
      meko: mekoRefs(q.meko),
    });
  }
  return out;
}

/** The questions file's JSON for a set of questions, as the design_questions tool saves it. */
export function questionsFileText(questions: DesignQuestion[]): string {
  return `${JSON.stringify({ questions }, null, 2)}\n`;
}

/**
 * The questions Claude gives the mod's design_questions tool (its `questions` input), checked
 * strictly, since Claude can fix them in the same turn: a list of objects, each with an id and a
 * question, ids not repeated, options a list of labels or {label, detail}, a recommendation that
 * points at an option, and a status of open or resolved. Returns them normalized as a questions
 * file would be (who decided each kept from `prev`, the questions kept before, when it isn't
 * given), or what's wrong.
 */
export function checkToolQuestions(input: unknown, prev: DesignQuestion[] = []): { questions: DesignQuestion[] } | { error: string } {
  if (!Array.isArray(input)) return { error: 'questions must be a list: {"questions": [{"id": …, "question": …}, …]}, or [] when there are none.' };
  const decisions = input.filter((v) => v && typeof v === 'object' && kindOf((v as Record<string, unknown>).kind) === 'decision').length;
  if (input.length - decisions > MAX_QUESTIONS) return { error: `Give at most ${MAX_QUESTIONS} questions.` };
  if (decisions > MAX_DECISIONS) return { error: `Give at most ${MAX_DECISIONS} decisions: only the ones that set the design's direction.` };
  const problems: string[] = [];
  const seen = new Set<string>();
  input.forEach((v, i) => {
    const at = `Question ${i + 1}`;
    if (!v || typeof v !== 'object' || Array.isArray(v)) {
      problems.push(`${at} must be an object.`);
      return;
    }
    const q = v as Record<string, unknown>;
    if (q.kind !== undefined && q.kind !== null && q.kind !== 'question' && q.kind !== 'decision') problems.push(`${at}: "kind" must be "question" or "decision".`);
    if (q.kind === 'decision') {
      if (!str(q.decision, DECISION_CHARS)) problems.push(`${at} is a decision, so it needs its "decision": what you chose.`);
      if (q.status !== undefined && q.status !== 'resolved') problems.push(`${at} is a decision, so its "status" must be "resolved".`);
    }
    if (q.decidedBy !== undefined && q.decidedBy !== null && q.decidedBy !== 'user' && q.decidedBy !== 'kode' && q.decidedBy !== 'agentk') problems.push(`${at}: "decidedBy" must be "user", "kode" or null.`);
    if (q.meko !== undefined && q.meko !== null) {
      if (!Array.isArray(q.meko)) problems.push(`${at}: "meko" must be a list.`);
      else {
        if (q.meko.length > MAX_MEKO_REFS) problems.push(`${at} cites ${q.meko.length} Meko items; give at most ${MAX_MEKO_REFS}.`);
        if (q.meko.some((r) => !mekoRef(r))) problems.push(`${at}: each "meko" item needs "kind" ("memory" or "knowledge") and the "id" Meko gave it.`);
      }
    }
    const id = questionId(q.id);
    if (!id) problems.push(`${at} needs an "id" (letters, digits and dashes).`);
    else if (seen.has(id)) problems.push(`${at} repeats the id "${id}".`);
    else seen.add(id);
    if (!str(q.question, QUESTION_CHARS)) problems.push(`${at} needs its "question" text.`);
    const options = q.options ?? [];
    if (!Array.isArray(options)) problems.push(`${at}: "options" must be a list.`);
    else {
      if (options.length > MAX_OPTIONS) problems.push(`${at} has ${options.length} options; give at most ${MAX_OPTIONS}.`);
      if (options.some((o) => !option(o))) problems.push(`${at}: each option needs a "label".`);
    }
    const rec = q.recommended ?? null;
    if (rec !== null && !(Number.isInteger(rec) && (rec as number) >= 0 && Array.isArray(options) && (rec as number) < options.length)) {
      problems.push(`${at}: "recommended" must be the index of one of its options (from 0), or null.`);
    }
    if (q.status !== undefined && q.status !== 'open' && q.status !== 'resolved') problems.push(`${at}: "status" must be "open" or "resolved".`);
  });
  if (problems.length) return { error: problems.slice(0, 10).join(' ') };
  const questions = parseDesignQuestions(JSON.stringify({ questions: input }), prev);
  if (Buffer.byteLength(questionsFileText(questions)) > MAX_QUESTIONS_FILE) {
    return { error: `The questions come to more than ${MAX_QUESTIONS_FILE / 1024} KB. Shorten their context and details.` };
  }
  return { questions };
}

/**
 * The questions file Kode writes beside the design: null when there's none (or it's empty);
 * throws when it's too large, can't be read, or isn't questions, so a bad file is never taken
 * for a missing one. `prev` is what `parseDesignQuestions` keeps who decided each from.
 */
export function readDesignQuestions(file: string, prev: DesignQuestion[] = []): DesignQuestion[] | null {
  let size: number;
  try {
    const st = statSync(file);
    if (!st.isFile()) throw new Error('not a file');
    size = st.size;
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw e;
  }
  if (size > MAX_QUESTIONS_FILE) throw new Error(`larger than ${MAX_QUESTIONS_FILE / 1024} KB`);
  const text = readFileSync(file, 'utf8');
  return text.trim() ? parseDesignQuestions(text, prev) : null;
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
    out.push({ id, kind: 'question', question, context: null, options: [], recommended: null, status: 'open', decision: null, decidedBy: null, meko: [] });
    if (out.length >= MAX_QUESTIONS) break;
  }
  return out;
}

/** Longest answer, in characters. */
export const ANSWER_MAX = 10_000;
/** Longest set of answers sent to Claude at once, in characters. */
export const ANSWERS_PROMPT_MAX = 50_000;

/**
 * "2 answers to open questions, a change to one of your decisions and other feedback", for the
 * prompt's first line. An answer to one of Kode's own decisions (in `questions`) is a change to it.
 */
export function answersCount(answers: DesignAnswerInput[], feedback: string, questions: DesignQuestion[] = []): string {
  const decisions = new Set(questions.filter((q) => q.kind === 'decision').map((q) => q.id));
  const changes = answers.filter((a) => decisions.has(a.questionId)).length;
  const n = answers.length - changes;
  const parts = [];
  if (n) parts.push(n === 1 ? '1 answer to an open question' : `${n} answers to open questions`);
  if (changes) parts.push(changes === 1 ? 'a change to one of your decisions' : `changes to ${changes} of your decisions`);
  if (feedback.trim()) parts.push(n || changes ? 'other feedback' : 'feedback');
  return parts.length > 2 ? `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}` : parts.join(' and ');
}

/** The user's answer as lines for the agent: the option (with any note) or the custom answer. */
function answerLines(a: DesignAnswerInput, lead: string): string[] {
  const text = a.text.trim();
  if (a.label === null) return [`${lead} (custom): ${text}`];
  return text ? [`${lead}: ${a.label}`, `Note: ${text}`] : [`${lead}: ${a.label}`];
}

/**
 * The answers as markdown for the agent: every open question in the design's order (numbered as
 * the user saw them, Q1…), answered or not, then each of Kode's own decisions the user changed
 * (D1…), then other feedback. A question without an answer is left to Kode's recommendation.
 */
export function formatAnswers(answers: DesignAnswerInput[], feedback: string, questions: DesignQuestion[]): string {
  const byId = new Map(answers.map((a) => [a.questionId, a]));
  const number = decisionNumbers(questions);
  const head = (q: DesignQuestion) => `### ${number.get(q.id)} (id: ${q.id}): ${q.question}`;
  const open = questions.flatMap((q) => {
    if (q.kind === 'decision' || q.status !== 'open') return [];
    const a = byId.get(q.id);
    if (a) return [[head(q), ...answerLines(a, 'Answer')].join('\n')];
    return [`${head(q)}\n${q.recommended !== null ? 'No answer: go with your recommendation.' : 'No answer: use your judgment, and say what you picked.'}`];
  });
  const changed = questions.flatMap((q) => {
    const a = q.kind === 'decision' ? byId.get(q.id) : undefined;
    return a ? [[head(q), `You decided: ${q.decision ?? ''}`, ...answerLines(a, 'The user wants')].join('\n')] : [];
  });
  const out = [...open, ...changed];
  if (feedback.trim()) out.push(`### Other feedback\n${feedback.trim()}`);
  return out.join('\n\n');
}

/**
 * A question resolved by the user's answer (`a`), as the questions file keeps it: the option
 * picked (with any note) or the custom answer as its decision, decided by the user. Without an
 * answer it's left to Kode, on its recommendation when it has one. One of Kode's own decisions
 * answered this way is the user's change to it.
 */
export function answeredQuestion(q: DesignQuestion, a: DesignAnswerInput | undefined): DesignQuestion {
  let decision: string;
  if (!a) {
    const rec = q.recommended !== null ? q.options[q.recommended] : undefined;
    decision = rec ? `Left to Kode: ${rec.label}` : 'Left to Kode';
  } else {
    const text = a.text.trim();
    decision = a.label === null ? text : text ? `${a.label} — ${text}` : a.label;
  }
  return { ...q, status: 'resolved', decision: clip(decision, DECISION_CHARS), decidedBy: a ? 'user' : 'kode' };
}
