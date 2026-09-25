import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { get, type Db } from './db.js';

/**
 * Instructions sent to Claude at each lifecycle step. A repo can override any
 * of them with .agent-k/stages/<name>.md; the owner can override them in the
 * stage_prompts table. Placeholders: {{task}}, {{branch}}, {{base}}, {{pr}},
 * {{prUrl}}, {{repo}}, {{reviewer}}.
 */
export type PromptName = 'design' | 'code' | 'pr' | 'review' | 'review-people' | 'continue' | 'continue-review';

export const DEFAULT_PROMPTS: Record<PromptName, string> = {
  design: `You are in the Design stage of an AgentK Local task.

Task: {{task}}

Read the relevant code, then write a design: the goal, the approach, the files you expect to change, risks and open questions, and a test plan. Don't change any code yet. When the design is ready, present it with ExitPlanMode so it can be approved.`,

  code: `Task: {{task}}

Implement this on the current branch ({{branch}}). Follow the repo's conventions (CLAUDE.md, contributing docs), add or update tests, and run the repo's tests and checks before you finish. Don't open a pull request yet.`,

  pr: `Open a pull request for this work.

1. Review the full diff against {{base}}. Make sure the repo's tests and checks pass.
2. Commit using this repo's commit conventions (see CLAUDE.md and any commit skill) on branch {{branch}}.
3. Push the branch to origin and run \`gh pr create --base {{base}}\` with a clear title and a description that summarizes the design and the change.

Finish by printing the PR URL.`,

  review: `A review from {{reviewer}} arrived on PR #{{pr}} ({{prUrl}}). Address it:

1. Read the review body (including any "could not be anchored" or "still open from the earlier round" notes) and every inline comment by {{reviewer}}: \`gh api repos/{{repo}}/pulls/{{pr}}/comments --paginate\`.
2. Verify each finding against the code before changing anything. If a finding is wrong, say why with evidence instead of changing working code.
3. Fix all valid findings in one commit (or a few coherent ones), run the repo's checks, and push. Update any docs your fixes make stale.
4. Reply briefly on every inline thread saying what was done: \`gh api -X POST repos/{{repo}}/pulls/{{pr}}/comments/<id>/replies -f body="..."\`.
5. React on the first comment of each finding: +1 if the finding was correct, -1 if it was wrong: \`gh api -X POST repos/{{repo}}/pulls/comments/<id>/reactions -f content='+1'\`.

Don't comment \`/agentk review\` after pushing. Requesting another round is the user's decision.`,

  'review-people': `Reviewers left feedback on PR #{{pr}} ({{prUrl}}). Read every review and inline comment (\`gh pr view {{pr}} --comments\` and \`gh api repos/{{repo}}/pulls/{{pr}}/comments --paginate\`), verify each point against the code, fix what's valid, run the repo's checks, push, and reply on each thread with what you did or why you disagree.`,

  continue: `The host restarted while you were in the middle of a turn, and this session was resumed. Before continuing, check \`git status\`, \`git log -3\` and the last commands you ran, so you don't repeat steps that already finished. Then continue where you left off.`,

  'continue-review': `The host restarted while you were in the middle of a turn, and this session was resumed. Before continuing, check \`git status\`, \`git log -3\`, whether your last push landed, and the current replies and reactions on PR #{{pr}} (\`gh api repos/{{repo}}/pulls/{{pr}}/comments --paginate\`), so you don't repeat finished steps or post duplicate replies. Then continue where you left off.`,
};

export function loadPrompt(db: Db, name: PromptName, worktree: string | null): string {
  if (worktree) {
    const f = join(worktree, '.agent-k', 'stages', `${name}.md`);
    if (existsSync(f)) {
      try {
        return readFileSync(f, 'utf8');
      } catch {
        /* fall through */
      }
    }
  }
  const row = get<{ template: string }>(db, 'SELECT template FROM stage_prompts WHERE stage = ?', name);
  return row?.template ?? DEFAULT_PROMPTS[name];
}

export function render(template: string, vars: Record<string, string | number | null | undefined>): string {
  return template.replace(/\{\{(\w+)\}\}/g, (_, k: string) => {
    const v = vars[k];
    return v === null || v === undefined ? '' : String(v);
  });
}
