/**
 * The shapes a review's steps report in, and the checks AgentK runs on what arrives: a pass's report
 * (`review_report`), a scorer's verdict (`review_score`) and the task's outcome for each finding
 * (`review_outcome`). The schemas are the agent-k reviewer's (`review_schema.json`,
 * `dependents_schema.json`, `claims_schema.json`, `score_schema.json`), except that a finding's
 * gotchas are the pass's own Meko finds, each credited with the sentence it instantiates.
 *
 * A required field is an instruction the model can't answer by not answering, which is the failure
 * prose mandates have (passes.go), so a report missing one is refused with the path at fault and the
 * step calls the tool again.
 */
import { SEVERITIES, type Severity } from '../../shared/codereview.js';
import type { Finding, Pass, ScoreReport } from './types.js';

type Schema = Record<string, unknown>;

const SEVERITY = { type: 'string', enum: SEVERITIES };

/** A finding's gotcha credits. Only credits with a well-formed id and a quote are kept (`checkReport`). */
const GOTCHAS: Schema = {
  type: 'array',
  description:
    'Meko gotchas from your own search of the gotchas datapack named in this prompt that this finding is a direct instance of. ' +
    "Before crediting one, locate the sentence in that gotcha's body that describes the failure, and satisfy yourself that this " +
    'finding exhibits the same mechanism; put that sentence, quoted exactly, in quote. If you cannot point to the sentence it ' +
    'instantiates, it is not a credit: the same file, the same subsystem, the same kind, a shared theme or category, and an ' +
    'adjacent concern are all near-misses. "both are races" and "both involve docs" are not credits, and a gotcha about data ' +
    'being silently dropped is not credited by a finding that documentation is unclear, even in the file the gotcha names. ' +
    'Empty when none qualify, which is most findings, including many the gotchas loosely resemble. Copy ids (MG-<n>) exactly ' +
    'from what the search returned; never invent one, and when unsure, credit none.',
  items: {
    type: 'object',
    required: ['id', 'quote'],
    properties: {
      id: { type: 'string', description: 'The gotcha\'s id, MG-<n>, as the search returned it.' },
      quote: { type: 'string', description: "The sentence of the gotcha's body this finding instantiates, quoted exactly." },
    },
  },
};

const FINDING: Schema = {
  type: 'object',
  required: ['file', 'line', 'severity', 'comment'],
  properties: {
    file: { type: 'string', description: 'Repo-relative path of a file this change touches.' },
    line: {
      type: 'integer',
      minimum: 1,
      description: 'Line number in the file as it is in the working tree. Must fall inside the diff.',
    },
    severity: SEVERITY,
    gotchas: GOTCHAS,
    comment: { type: 'string', description: 'What is wrong and why it matters. Terse and actionable; no restating the diff.' },
  },
};

const SUMMARY: Schema = {
  type: 'string',
  description:
    'Shown above the findings. A real problem that is not already one of the findings, or exactly "LGTM" when there is none — ' +
    'which is the usual case. Never a description of the change or of what was checked.',
};

const FINDINGS: Schema = {
  type: 'array',
  description: 'Line-anchored review comments. Empty if the change looks clean.',
  items: FINDING,
};

/** The shared `{ summary, findings }` report. */
export const REVIEW_SCHEMA: Schema = {
  type: 'object',
  required: ['summary', 'findings'],
  properties: { summary: SUMMARY, findings: FINDINGS },
};

/** The dependents pass's report: the state ledger first, then the verdict (field order is load-bearing). */
export const DEPENDENTS_SCHEMA: Schema = {
  type: 'object',
  required: ['state_ledger', 'summary', 'findings'],
  properties: {
    state_ledger: {
      type: 'array',
      description: 'One row per piece of state the changed functions create, destroy, reset, reorder or make conditional. Fill this in before concluding anything.',
      items: {
        type: 'object',
        required: ['what', 'where', 'later_consumers', 'evidence', 'survivors', 'still_holds'],
        properties: {
          what: { type: 'string', description: 'The state created, destroyed, reset, reordered or made conditional.' },
          where: { type: 'string', description: 'file:line of the operation, on the changed side.' },
          later_consumers: {
            type: 'string',
            description:
              'Everything that reads or depends on this state afterward. Walk the enclosing function to its last line first, in ' +
              'execution order, then go outward to callers, callees and tests. Name file:line for each. Write \'none found\' only after looking.',
          },
          evidence: { type: 'string', description: 'The files and line ranges you actually opened for this row.' },
          survivors: {
            type: 'array',
            description:
              'Only for state this row RESETS, DROPS, CLEARS, RECREATES, TRUNCATES or INVALIDATES. Every place that holds a ' +
              'reference, id, handle, path, offset, cache or copy derived from the old contents and is NOT itself reset by this ' +
              'change — necessarily in a different store: on-disk config, a pidfile, a struct field loaded at startup, another ' +
              'database, a remote service, an open connection. An empty array is a claim that you went looking and found none.',
            items: {
              type: 'object',
              required: ['survivor', 'where', 'evidence', 'used_next', 'reconciled_by', 'reconciled', 'anchor_file', 'anchor_line', 'finding', 'severity'],
              properties: {
                survivor: { type: 'string', description: 'The surviving reference and what it points at in the store that was reset.' },
                where: { type: 'string', description: 'file:line where it is stored, and file:line where it is next read after the reset.' },
                evidence: { type: 'string', description: 'The files and line ranges you opened looking for what reconciles this.' },
                used_next: {
                  type: 'string',
                  description:
                    'Trace the first use of this survivor after the reset. Name the code that reads it, with file:line, and say ' +
                    'concretely what it will find in the store that was reset — the row, key, file or entry it goes looking for, ' +
                    'and whether that specific thing is back.',
                },
                reconciled_by: {
                  type: 'string',
                  description:
                    'Name the code that re-derives, re-validates, clears or rejects this survivor after the reset, with its ' +
                    'file:line. Rebuilding the store empty is NOT reconciliation: recreating a database, running its migrations, ' +
                    'recreating a directory or reinitialising a cache restores the shape of the store, not the contents the ' +
                    'survivor points at, and the survivor is still dangling. Neither is a comment, a commit message, a PR ' +
                    'description, or an assumption about how the tool is normally run. If you found no such code, write ' +
                    '\'none found\' and say where you looked.',
                },
                reconciled: {
                  type: 'boolean',
                  description: 'True only if reconciled_by names actual code, at a file:line you read, that acts on this survivor. Anything else is false.',
                },
                anchor_file: {
                  type: 'string',
                  description: 'Repo-relative path of the changed file performing the reset. Must be a file this change touches.',
                },
                anchor_line: { type: 'integer', description: 'Line on the changed side where the reset happens. This is where the comment goes.' },
                finding: {
                  type: 'string',
                  description:
                    'Write this for every survivor, including the ones you concluded are reconciled. Address the author directly: ' +
                    'what survives the reset, what it still points at in the store that was reset, and what happens when the code ' +
                    'named in used_next reaches it. If reconciled_by names real code, say which line does the reconciling. Write ' +
                    'only about the code. Whether this is worth raising is decided elsewhere and is not yours to weigh in on, so do ' +
                    'not qualify it, hedge it, or say whether you are reporting it — a sentence about your own filing decision is ' +
                    'the one sentence that must not be here.',
                },
                severity: {
                  ...SEVERITY,
                  description:
                    'What it costs when the code named in used_next reaches this survivor. Rate the consequence, not how sure you ' +
                    'are: data loss, corruption, or a daemon that cannot start is a blocker; a confusing error or a path that ' +
                    'recovers on a retry is major or minor.',
                },
              },
            },
          },
          still_holds: {
            type: 'boolean',
            description: 'Whether every consumer named above, and every survivor, is still correct after this change. Answer this last, from what you wrote above it.',
          },
        },
      },
    },
    summary: SUMMARY,
    findings: { type: 'array', items: FINDING },
  },
};

/** The claims pass's report: the claims ledger first, then the verdict. */
export const CLAIMS_SCHEMA: Schema = {
  type: 'object',
  required: ['claims', 'summary', 'findings'],
  properties: {
    claims: {
      type: 'array',
      description:
        'One row per assertion made in or around the changed lines: comments, doc comments, log lines, error messages, the ' +
        "task's title and description, the commit messages. Enumerate them before judging any of them.",
      items: {
        type: 'object',
        required: ['claim', 'where', 'asserts', 'checked', 'contradicted_by', 'holds', 'anchor_file', 'anchor_line', 'finding', 'severity'],
        properties: {
          claim: { type: 'string', description: 'The assertion, quoted.' },
          where: {
            type: 'string',
            description: "file:line of the comment, log line or message. For the task's title or description, or a commit message, say which.",
          },
          asserts: {
            type: 'string',
            description: 'What has to be true of the code for this claim to be true. State it as something checkable, not as a restatement of the claim.',
          },
          checked: {
            type: 'string',
            description:
              'The files and line ranges you opened to check it. Check the claim against the code, not against the rest of the ' +
              'prose — an author who was wrong once tends to have written it down twice.',
          },
          contradicted_by: {
            type: 'string',
            description:
              'The code that makes this claim false, out of date, or true only under a condition nothing enforces, with its ' +
              'file:line. Look hardest at claims that something is safe, local, deliberate, cheap, temporary, already handled ' +
              'elsewhere, or done the same way as before: a wrong assumption hides best in a sentence a reader would go to in ' +
              "order to confirm it. Write 'none found' if the claim holds.",
          },
          holds: { type: 'boolean', description: "True only if contradicted_by is 'none found'. Answer this from what you wrote above it." },
          anchor_file: { type: 'string', description: 'Repo-relative path of the changed file to comment on. Must be a file this change touches.' },
          anchor_line: { type: 'integer', description: 'Line on the changed side to anchor the comment to.' },
          finding: {
            type: 'string',
            description:
              'Empty if the claim holds. Otherwise address the author directly: which claim, and what in the code contradicts ' +
              'it. Write only about the code — do not hedge it, qualify it, or say whether you are reporting it.',
          },
          severity: {
            ...SEVERITY,
            description:
              'Ignored when the claim holds. Otherwise, how much the gap between the claim and the code costs a reader. Rate the ' +
              'consequence, not how sure you are: a comment naming a function that does not exist misleads whoever reads it next ' +
              'and is minor; a claim that something is guarded, safe or already handled when it is not sends a reader away from a ' +
              'real bug and is major or worse.',
          },
        },
      },
    },
    summary: SUMMARY,
    findings: { type: 'array', items: FINDING },
  },
};

/** A scorer's verdict: what it read and the refutation first, the verdict after. */
export const SCORE_SCHEMA: Schema = {
  type: 'object',
  required: ['read', 'refutation', 'verdict', 'confidence', 'reason'],
  properties: {
    read: {
      type: 'array',
      description:
        'Every file you actually opened to reach your verdict, as "path:start-end". A grep hit is not a read. An empty list means ' +
        'you did not check, and a verdict with no read behind it cannot drop a finding.',
      items: { type: 'string' },
    },
    refutation: {
      type: 'string',
      description:
        'The fact in the code that makes this finding wrong, with the file:line you read it at — the guard that already exists, ' +
        'the caller that cannot reach it, the line this change did not touch. "It seems unlikely" is not a refutation. Empty if ' +
        'you did not find one. Write this before you answer verdict.',
    },
    verdict: {
      type: 'string',
      enum: ['refuted', 'confirmed', 'unverified'],
      description:
        'refuted only if refutation above names a real fact you read. confirmed: you read the code and the finding holds. ' +
        'unverified: you could not establish either way.',
    },
    confidence: {
      type: 'integer',
      minimum: 0,
      maximum: 100,
      description: "How confident you are that this finding is real and worth the author's time.",
    },
    reason: { type: 'string', description: 'One or two sentences for the verdict, naming what you read.' },
  },
};

/** What the task did with each finding of a round (`review_outcome`). */
export const OUTCOME_SCHEMA: Schema = {
  type: 'object',
  required: ['outcomes'],
  properties: {
    outcomes: {
      type: 'array',
      minItems: 1,
      items: {
        type: 'object',
        required: ['key', 'status', 'note'],
        properties: {
          key: { type: 'string', description: "The finding's key, as the review's prompt gave it." },
          status: { type: 'string', enum: ['fixed', 'declined'] },
          note: { type: 'string', description: 'What you changed, or, for a declined finding, why it is wrong or not worth changing.' },
        },
      },
    },
  },
};

/**
 * The first place `v` doesn't meet `schema`, as `path: problem`, or null when it does. Enough of
 * JSON schema for the ones above: type, required, enum, properties, items, minimum, maximum and
 * minItems. Keys a schema doesn't name are left alone; nothing reads them.
 */
export function schemaError(schema: Schema, v: unknown, path = '$'): string | null {
  const type = schema.type as string | undefined;
  switch (type) {
    case 'object': {
      if (!v || typeof v !== 'object' || Array.isArray(v)) return `${path}: must be an object`;
      const o = v as Record<string, unknown>;
      for (const k of (schema.required as string[] | undefined) ?? []) {
        if (!(k in o) || o[k] === undefined || o[k] === null) return `${path}.${k}: is required`;
      }
      const props = (schema.properties as Record<string, Schema> | undefined) ?? {};
      for (const [k, sub] of Object.entries(props)) {
        if (o[k] === undefined || o[k] === null) continue;
        const e = schemaError(sub, o[k], `${path}.${k}`);
        if (e) return e;
      }
      return null;
    }
    case 'array': {
      if (!Array.isArray(v)) return `${path}: must be an array`;
      const min = schema.minItems as number | undefined;
      if (min !== undefined && v.length < min) return `${path}: needs at least ${min} item${min === 1 ? '' : 's'}`;
      const items = schema.items as Schema | undefined;
      if (items) {
        for (let i = 0; i < v.length; i++) {
          const e = schemaError(items, v[i], `${path}[${i}]`);
          if (e) return e;
        }
      }
      return null;
    }
    case 'string':
      if (typeof v !== 'string') return `${path}: must be a string`;
      break;
    case 'boolean':
      if (typeof v !== 'boolean') return `${path}: must be true or false`;
      return null;
    case 'integer':
      if (typeof v !== 'number' || !Number.isInteger(v)) return `${path}: must be a whole number`;
      break;
    case 'number':
      if (typeof v !== 'number' || !Number.isFinite(v)) return `${path}: must be a number`;
      break;
  }
  const allowed = schema.enum as unknown[] | undefined;
  if (allowed && !allowed.includes(v)) return `${path}: must be one of ${allowed.map((a) => JSON.stringify(a)).join(', ')}`;
  if (typeof v === 'number') {
    if (typeof schema.minimum === 'number' && v < schema.minimum) return `${path}: must be at least ${schema.minimum}`;
    if (typeof schema.maximum === 'number' && v > schema.maximum) return `${path}: must be at most ${schema.maximum}`;
  }
  return null;
}

/** A gotcha id as the cloud's miner writes them. */
const GOTCHA_ID = /^MG-[1-9][0-9]*$/;

/** A path as a finding gives it, as the diff spells it. */
export function cleanPath(p: string): string {
  return p.trim().replace(/^(\.\/)+/, '');
}

function finding(raw: Record<string, unknown>): Finding {
  const credits = Array.isArray(raw.gotchas) ? (raw.gotchas as Record<string, unknown>[]) : [];
  const gotchas: string[] = [];
  for (const g of credits) {
    const id = typeof g.id === 'string' ? g.id.trim() : '';
    const quote = typeof g.quote === 'string' ? g.quote.trim() : '';
    // A credit without the sentence it instantiates is the near-miss the rules forbid.
    if (GOTCHA_ID.test(id) && quote && !gotchas.includes(id)) gotchas.push(id);
  }
  return {
    file: cleanPath(raw.file as string),
    line: raw.line as number,
    severity: raw.severity as Severity,
    comment: (raw.comment as string).trim(),
    gotchas,
  };
}

/**
 * A ledger row's write-up names a place in the code (`ledger.go`'s citesCode): a reconciliation
 * the dependents pass claims without one doesn't clear the author.
 */
const CITES_CODE = /[\w./-]+\.\w+:\d+/;

interface LedgerRow {
  reconciled?: boolean;
  reconciled_by?: string;
  holds?: boolean;
  anchor_file?: string;
  anchor_line?: number;
  finding?: string;
  severity?: string;
}

/** Every row of whichever ledger a report carries (`ledgerRows`). */
function ledgerRows(report: Record<string, unknown>): LedgerRow[] {
  const rows: LedgerRow[] = Array.isArray(report.claims) ? [...(report.claims as LedgerRow[])] : [];
  if (Array.isArray(report.state_ledger)) {
    for (const s of report.state_ledger as { survivors?: LedgerRow[] }[]) rows.push(...(s.survivors ?? []));
  }
  return rows;
}

/**
 * One finding per ledger row the pass didn't clear with a citation (`ledgerFindings`): the ledger
 * enumerates and the scorer decides, so an uncited "this is fine" costs a scoring call rather than
 * the finding. A claim's `holds` is taken at its word; a survivor's `reconciled` needs a file:line.
 */
export function ledgerFindings(report: Record<string, unknown>): Finding[] {
  const out: Finding[] = [];
  for (const r of ledgerRows(report)) {
    const clean = r.holds === true || (r.reconciled === true && CITES_CODE.test(r.reconciled_by ?? ''));
    const text = (r.finding ?? '').trim();
    if (clean || !text || !r.anchor_file || !r.anchor_line || r.anchor_line <= 0) continue;
    const sev = SEVERITIES.includes(r.severity as Severity) ? (r.severity as Severity) : 'major';
    out.push({ file: cleanPath(r.anchor_file), line: r.anchor_line, severity: sev, comment: text, gotchas: [] });
  }
  return out;
}

/**
 * A pass's report checked against its schema: its summary and findings, the ledger's uncleared
 * rows among them; or the first thing wrong with it, for the pass to fix and report again.
 */
export function checkReport(pass: Pass, input: unknown): { summary: string; findings: Finding[] } | { error: string } {
  const e = schemaError(pass.schema, input, 'report');
  if (e) return { error: e };
  const report = input as Record<string, unknown>;
  const findings = (report.findings as Record<string, unknown>[]).map(finding);
  return { summary: (report.summary as string).trim(), findings: [...findings, ...ledgerFindings(report)] };
}

/** A scorer's verdict checked against SCORE_SCHEMA. */
export function checkScore(input: unknown): ScoreReport | { error: string } {
  const e = schemaError(SCORE_SCHEMA, input, 'score');
  if (e) return { error: e };
  const s = input as ScoreReport;
  return {
    read: s.read.map((r) => r.trim()).filter(Boolean),
    refutation: s.refutation.trim(),
    verdict: s.verdict,
    confidence: s.confidence,
    reason: s.reason.trim(),
  };
}

export interface OutcomeInput {
  key: string;
  status: 'fixed' | 'declined';
  note: string;
}

/** The task's outcomes checked against OUTCOME_SCHEMA. */
export function checkOutcomes(input: unknown): OutcomeInput[] | { error: string } {
  const e = schemaError(OUTCOME_SCHEMA, input, 'input');
  if (e) return { error: e };
  return (input as { outcomes: OutcomeInput[] }).outcomes.map((o) => ({ key: o.key.trim(), status: o.status, note: o.note.trim() }));
}
