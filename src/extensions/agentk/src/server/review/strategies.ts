/**
 * The review strategies: which passes run, what each is told to look for, and on which tier. The
 * Sagittarius 3.0 mandates are the agent-k reviewer's (`pkg/agenticreview/passes.go` at 74e35b9),
 * word for word: changing one changes what a review looks for, so it needs a new strategy id
 * (`src/shared/codereview.ts`), or the reviews before and after it can't be told apart.
 *
 * The mandates say "this PR": the base prompt tells the passes that means the change on the task's
 * branch, whose PR isn't open yet (prompt.ts).
 */
import { DEFAULT_STRATEGY, strategyInfo, type ReviewStrategyId } from '../../shared/codereview.js';
import { CLAIMS_SCHEMA, DEPENDENTS_SCHEMA, REVIEW_SCHEMA } from './schemas.js';
import type { Pass, Strategy } from './types.js';

/**
 * What every pass of a multi-pass strategy is told after its mandate (`Pass.Prompt`): the narrow
 * mandates overlap at the edges on purpose, and the merge is the orchestrator's.
 */
export const PASS_FOOTER = `Other passes cover the rest of the review. Report what falls in your
assignment and leave the rest alone — a finding outside it is noise that
another pass is already responsible for. Reporting nothing is a normal
outcome for a pass.`;

const GUIDE = `Hold this PR to **the repository's own review guide** — the file named at
the top of this prompt. Re-read it, then go rule by rule.

Report only what the guide actually asks for, and name the rule each finding
rests on. A rule you are extrapolating is not a rule. If the guide is silent
on something, it is not yours this pass.`;

const DIFF = `Read **the changed lines themselves**, closely, and stay in them.

You are the pass that catches what is wrong on the face of it: an inverted
condition, an off-by-one, an error or nil path that is dropped, a resource
never closed, a shadowed variable, an unreachable branch, a lock not
released, a loop variable captured by reference.

Do not go exploring. If deciding whether something is a bug requires reading
half the repository, it belongs to a different pass. Report only what is
visible in the diff.`;

const DEPENDENTS = `Find **what else in this repository depends on the behavior this PR
changes**, and decide whether each dependent still holds.

Fill in the state ledger your output schema requires before you conclude
anything. The ledger is not a write-up of the review — it is the review. A row
whose consumers you did not go and open is a row you have not done.

Start inside the enclosing function, before you grep for anything. Read every
changed function from its first line to its last, including the parts the diff
does not show, and ask what the code *after* the change relies on that the
change has just created, destroyed, reordered, or made conditional. The
nearest dependent is usually a few lines further down in the same function,
and it is the one a symbol search will never surface, because there is no
symbol to search for.

Only then go outward: the callers, the callees, anything that reads state this
writes, the tests that claim to cover it. Do not reason about what *should*
depend on this — grep for what does.

Give separate attention to what the change **does not** touch. When a change
resets, drops, clears, recreates, truncates or invalidates a store, the bug is
usually not in the store it reset — that one is now consistent with itself. It
is in whatever kept pointing at the old contents and was not reset alongside
it, because it lives somewhere else: an id persisted to disk, a path, an
offset, a cached handle, a struct field loaded from a config file at startup,
a row in a different database. Those survive, and the code goes on using them
as though they were still valid. Fill in the survivors list for every such row,
and to call one reconciled you have to point at the code that re-derives,
clears or rejects it. A comment or a commit message saying it happens is not
that code.

Report a finding when a dependent breaks, or is left holding an assumption
the change has quietly invalidated. Anchor it to the changed line that causes
the break, not to the dependent that suffers it.`;

const HISTORY = `Judge this change **against what the code has already been through**.

The prompt lists, per changed file, the commits that last wrote the lines this
PR rewrites and what has recently happened to the file. Read those subjects,
then read the code they describe.

What you are looking for: a guard, a retry, a special case, or an ordering that
some earlier commit added deliberately, and that this PR removes, weakens, or
steps around without saying why. Code that looks redundant often is not — it
looks that way precisely because the incident that caused it is not in the
diff. The same goes for a file that has been fixed repeatedly in one spot; that
is where the next bug goes.

Verify before you report. A commit subject is a lead, not evidence — go read
what the commit actually left in the code and confirm this PR undoes it.

Report a finding when this PR reverts or defeats something an earlier commit
put there on purpose. Say which commit, what it was for, and what now bypasses
it. If the removal is deliberate and the PR says so, that is not a finding.`;

const CLAIMS = `Check whether **the code does what it says it does**.

Every comment, doc comment, log line, and error message in and around the
changed lines is a claim by the author, as are the PR title, the description,
and the commit messages. Enumerate them in the claims ledger your output schema
requires, then check each one against the code.

Give the most attention to a claim that something is safe, local, deliberate,
cheap, temporary, or already handled elsewhere. A wrong assumption hides best
in exactly that kind of sentence, because it sits where a reviewer would go to
confirm it and reading it feels like having checked it.

Report a finding when a claim is false, has stopped being true because of this
change, or is true only under a condition nothing actually enforces. Say which
claim, and what in the code contradicts it.`;

/**
 * Sirius 1.0's one pass: the five assignments in one, for a cheaper review. It reports through the
 * shared schema, without the ledgers, and isn't scored, so it's told to raise only what it checked.
 */
const ALL = `You are the whole review: no other pass runs. Cover each of these in turn,
and report what you find under any of them.

1. **The repository's own review guide** — the file named at the top of this
   prompt. Go rule by rule, and name the rule each finding rests on.
2. **The changed lines themselves**: an inverted condition, an off-by-one, an
   error or nil path that is dropped, a resource never closed, a shadowed
   variable, an unreachable branch, a lock not released.
3. **What depends on the behavior this PR changes**: read each changed function
   to its last line, then its callers, callees and tests, and decide whether
   each still holds. When the change resets or recreates a store, look for what
   kept pointing at the old contents somewhere else.
4. **What the code has already been through**: a guard, retry, special case or
   ordering an earlier commit added on purpose that this PR removes or steps
   around without saying why. A commit subject is a lead, not evidence.
5. **Whether the code does what it says it does**: the comments, log lines,
   error messages, title, description and commit messages are claims. Check
   the ones that call something safe, local, deliberate, cheap, temporary or
   already handled elsewhere.

No scorer checks your findings after you, so report only what you verified in
the code. Reporting nothing is a normal outcome.`;

const SAGITTARIUS: Pass[] = [
  { name: 'guide', tier: 'large', mandate: GUIDE, schema: REVIEW_SCHEMA },
  { name: 'diff', tier: 'small', mandate: DIFF, schema: REVIEW_SCHEMA },
  { name: 'dependents', tier: 'large', mandate: DEPENDENTS, schema: DEPENDENTS_SCHEMA },
  { name: 'history', tier: 'large', mandate: HISTORY, schema: REVIEW_SCHEMA },
  { name: 'claims', tier: 'large', mandate: CLAIMS, schema: CLAIMS_SCHEMA },
];

const SIRIUS: Pass[] = [{ name: 'all', tier: 'mid', mandate: ALL, schema: REVIEW_SCHEMA, alone: true }];

const PASSES: Record<ReviewStrategyId, Pass[]> = {
  'sagittarius-3.0': SAGITTARIUS,
  'sirius-1.0': SIRIUS,
};

/**
 * The strategy for an id. An unknown one (a config typo, a rollback past the version that added it)
 * resolves to the default rather than refusing to review; the review records what actually ran.
 */
export function strategyFor(id: string): Strategy {
  const info = strategyInfo(id) ?? strategyInfo(DEFAULT_STRATEGY)!;
  return { id: info.id, name: info.name, passes: PASSES[info.id], scored: info.scored };
}
