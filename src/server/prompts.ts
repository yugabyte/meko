import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { get, type Db } from './db.js';

/**
 * Instructions sent to Claude at each lifecycle step. A repo can override any
 * of them with .agent-k/stages/<name>.md; the owner can override them in the
 * stage_prompts table. Placeholders: {{task}}, {{branch}}, {{base}}, {{pr}},
 * {{prUrl}}, {{repo}}, {{reviewer}}, {{designFile}}; `comments` also gets {{count}} and
 * {{comments}}, and `ci` gets {{sha}} and {{checks}}.
 */
export type PromptName =
  | 'explore' | 'design' | 'design-plan' | 'approved' | 'code' | 'pr' | 'review' | 'review-people' | 'ci' | 'continue' | 'continue-review' | 'comments';

export const DEFAULT_PROMPTS: Record<PromptName, string> = {
  explore: `You are in an AgentK Local Exploration session.

Task: {{task}}

Investigate: read the code, answer questions, and build POCs or run experiments in this worktree (branch {{branch}}) when they help. You can commit and push experimental branches. There is no design approval in this session, and AgentK Local doesn't run a PR, review or merge stage for it.

Finish with a short summary of what you found and what you'd recommend next. Save durable findings to Meko.`,

  design: `You are in the Design stage of an AgentK Local task.

Task: {{task}}

Read the relevant code, then design the change: the goal, the approach, the files you expect to change, risks and open questions, and a test plan. Don't change anything in the repo yet: no file edits, commits, or commands that modify files. AgentK Local blocks edits in the repo until the user approves the design.

Put the design in exactly one place:
- If the design is complex (several components, a data flow, UI, or a state machine) and you have the Artifact tool, publish it as an Artifact, with diagrams where they help. Don't also write a markdown copy.
- Otherwise, write it as markdown to {{designFile}}.

Then stop and say the design is ready. The user reviews and approves it in AgentK Local. If they send feedback, revise the same Artifact (republish the same file) or the same markdown file.`,

  // For sessions created before designs moved out of plan mode.
  'design-plan': `You are in the Design stage of an AgentK Local task.

Task: {{task}}

Read the relevant code, then write a design: the goal, the approach, the files you expect to change, risks and open questions, and a test plan. Don't change any code yet. When the design is ready, present it with ExitPlanMode so it can be approved.`,

  approved: `The design is approved. Implement it on the current branch ({{branch}}). Follow the repo's conventions (CLAUDE.md, contributing docs), add or update tests, and run the repo's tests and checks before you finish. Don't open a pull request yet.`,

  code: `Task: {{task}}

Implement this on the current branch ({{branch}}). Follow the repo's conventions (CLAUDE.md, contributing docs), add or update tests, and run the repo's tests and checks before you finish. Don't open a pull request yet.`,

  pr: `Open a pull request for this work.

1. Review the full diff against {{base}}. Make sure the repo's tests and checks pass.
2. Commit using this repo's commit conventions (see CLAUDE.md and any commit skill) on branch {{branch}}.
3. Push the branch to origin. If the branch has no open PR yet, run \`gh pr create --base {{base}}\` with a clear title and a description that summarizes the design and the change; if it already has one (\`gh pr view\`), update that PR's title and description instead of opening another.

Finish by printing the PR URL.`,

  review: `A review from {{reviewer}} arrived on PR #{{pr}} ({{prUrl}}). Address it:

1. Read the review body (including any "could not be anchored" or "still open from the earlier round" notes) and every inline comment by {{reviewer}}: \`gh api repos/{{repo}}/pulls/{{pr}}/comments --paginate\`.
2. Verify each finding against the code before changing anything. If a finding is wrong, say why with evidence instead of changing working code.
3. Fix all valid findings in one commit (or a few coherent ones), run the repo's checks, and push. Update any docs your fixes make stale.
4. Reply briefly on every inline thread saying what was done: \`gh api -X POST repos/{{repo}}/pulls/{{pr}}/comments/<id>/replies -f body="..."\`.
5. React on the first comment of each finding: +1 if the finding was correct, -1 if it was wrong: \`gh api -X POST repos/{{repo}}/pulls/comments/<id>/reactions -f content='+1'\`.

Don't comment \`/agentk review\` after pushing. Requesting another round is the user's decision.`,

  'review-people': `Reviewers left feedback on PR #{{pr}} ({{prUrl}}). Read every review and inline comment (\`gh pr view {{pr}} --comments\` and \`gh api repos/{{repo}}/pulls/{{pr}}/comments --paginate\`), verify each point against the code, fix what's valid, run the repo's checks, push, and reply on each thread with what you did or why you disagree.`,

  ci: `CI failed on PR #{{pr}} ({{prUrl}}) at commit {{sha}}:
{{checks}}

1. If the PR's head has moved past {{sha}}, check CI on the new head first (\`gh pr checks {{pr}}\`), and stop if it's already passing or still running.
2. Read the failing jobs' logs: \`gh run view <run-id> --log-failed\` (the run id is in each link).
3. Find the cause. If it's unrelated to this change (a flaky test, a runner outage), don't change code: say so and rerun the failed jobs once with \`gh run rerun <run-id> --failed\`.
4. Otherwise fix it, run the repo's checks locally, commit and push. Update any docs the fix makes stale.

Don't comment \`/agentk review\`. Requesting another round is the user's decision.`,

  continue: `The host restarted while you were in the middle of a turn, and this session was resumed. Before continuing, check \`git status\`, \`git log -3\` and the last commands you ran, so you don't repeat steps that already finished. Then continue where you left off.`,

  'continue-review': `The host restarted while you were in the middle of a turn, and this session was resumed. Before continuing, check \`git status\`, \`git log -3\`, whether your last push landed, and the current replies and reactions on PR #{{pr}} (\`gh api repos/{{repo}}/pulls/{{pr}}/comments --paginate\`), so you don't repeat finished steps or post duplicate replies. Then continue where you left off.`,

  comments: `The user reviewed your changes in AgentK Local and left {{count}}. Address each one: verify it against the code, make the change or say why you disagree, and run the repo's checks. If this branch has an open PR, commit and push the fixes. Finish with a short list: each comment and what you did.

{{comments}}`,
};

/**
 * Added to the PR prompt when the project has a Meko datapack: the lines that end the PR
 * description, so the PR points back at the datapack and at the session's Meko conversation.
 */
export function mekoPrFooter(host: string, datapackId: string, conversationId: string | null): string {
  const lines = [`${host}://${datapackId}`];
  if (conversationId) lines.push(`meko_conversation_id:${conversationId}`);
  return `End the PR description with ${lines.length === 1 ? 'this line' : 'these lines'}, exactly as written and each on its own line, after everything else. Keep ${lines.length === 1 ? 'it' : 'them'} there if you update the description later.

${lines.join('\n')}`;
}

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
