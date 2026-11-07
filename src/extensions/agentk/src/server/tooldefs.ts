/**
 * Kode's session tools as an agent sees them: the `kode_sessions` MCP server's tools, with
 * their descriptions and input schemas. Kept on their own, with no imports, so `me_kode mcp`
 * (mcp.ts) loads nothing else; the server answers them in actions.ts (`SessionTools.call`).
 */

/** The MCP server's name in every harness's config: Claude Code's tools are `mcp__kode_sessions__<tool>`, Kiro's `@kode_sessions/<tool>`. */
export const TOOLS_SERVER = 'kode_sessions';

/** The tools' input, and the server's answers, change only in ways this version number tells apart. */
export const TOOLS_VERSION = 1;

export type ToolName = 'list' | 'start_sessions' | 'move_session' | 'open_pr' | 'open_project' | 'review_report' | 'review_score' | 'review_outcome';

export interface ToolDef {
  name: ToolName;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, unknown>; required?: string[] };
  /** It changes nothing, so it's pre-allowed (each harness's allow list). */
  readOnly?: boolean;
}

const STAGES = ['explore', 'design', 'code', 'review'];

export const TOOL_DEFS: ToolDef[] = [
  {
    name: 'list',
    readOnly: true,
    description:
      "Lists Kode's open projects (id, name, repo folder and GitHub owner/name) and their running sessions (id, name, stage, " +
      'status, branch, PR), with this session marked self, and the agents new sessions can start on (the default marked, with ' +
      'the models each takes). Call it first, and use only the ids it gives; see the kode-sessions ' +
      'skill for how to use these tools.',
    inputSchema: { type: 'object', properties: { projectId: { type: 'string', description: 'Only this project.' } } },
  },
  {
    name: 'start_sessions',
    description:
      'Starts up to 8 Kode sessions in one call, in order. Each is either a GitHub issue or PR (ref: owner/name#N or its link; ' +
      'for a bare #N use the project\'s github from list), which starts a task on the issue or a review of the PR, or a task ' +
      "(name and prompt, the user's words kept). Each runs on the default agent and its own model unless the user names " +
      "another (agent, model; list names the agents and their models). Returns one line per session: what started, or why it didn't.",
    inputSchema: {
      type: 'object',
      properties: {
        sessions: {
          type: 'array',
          minItems: 1,
          maxItems: 8,
          items: {
            type: 'object',
            properties: {
              projectId: { type: 'string', description: "The project to start it in. Default: this session's project." },
              ref: { type: 'string', description: 'A GitHub issue or PR: owner/name#N or its link.' },
              name: { type: 'string', description: 'A task without a ref: its short title.' },
              prompt: { type: 'string', description: "A task without a ref: what to do, in the user's words." },
              stage: { type: 'string', enum: ['explore', 'design', 'code'], description: "Where a task starts (an issue's: design or code). Default design." },
              branch: {
                type: 'string',
                description: "A task without a ref: an existing branch (in the project's repo, or on its origin) to work on as it is, " +
                  'instead of a new branch. Default: a new branch.',
              },
              agent: {
                type: 'string', enum: ['claude', 'opencode', 'kiro'],
                description: 'The coding agent it runs on, only when the user names one (list says which are available). Default: the default agent.',
              },
              model: {
                type: 'string',
                description: "The model its agent runs on, only when the user names one: as they said it, or a name list gives for the agent " +
                  "(such as opus or sonnet for Claude Code, provider/model for OpenCode). Default: the agent's own.",
              },
              autoRun: {
                type: 'boolean',
                description: 'Run to merge: Kode opens the PR when the coding turn ends and merges it once nothing blocks it. ' +
                  'Only when the user asks for it, and only for a task (or an issue) with stage code.',
              },
              review: {
                type: 'object',
                description: "The task's own code review before its PR opens, only what the user asked for (the rest keeps the " +
                  'project\'s settings): strategy (off for no review, sirius-1.0, or sagittarius-3.0), rounds (re-reviews after the ' +
                  'first, 0 to 5), depth (sweeps per review, 1 to 5: "3 deep reviews" is depth 3) and check (false: the findings go to ' +
                  'Kode without waiting for the user to check them).',
                properties: {
                  strategy: { type: 'string', enum: ['off', 'sirius-1.0', 'sagittarius-3.0'] },
                  rounds: { type: 'integer', minimum: 0, maximum: 5 },
                  depth: { type: 'integer', minimum: 1, maximum: 5 },
                  check: { type: 'boolean' },
                },
              },
            },
          },
        },
      },
      required: ['sessions'],
    },
  },
  {
    name: 'move_session',
    description:
      "Moves a Kode session to another stage (explore, design, code, or review for Pull Request), as the user's move dialog " +
      "does. This session's own move goes once this turn ends, so ask for it last. instructions: what the user asked for in " +
      'that stage, if anything.',
    inputSchema: {
      type: 'object',
      properties: {
        sessionId: { type: 'string' },
        stage: { type: 'string', enum: STAGES },
        instructions: { type: 'string' },
      },
      required: ['sessionId', 'stage'],
    },
  },
  {
    name: 'open_pr',
    description:
      "Opens the PR of another Kode session in Code, as its Open PR does. Not for this session: the user opens this session's PR. " +
      'instructions: what the user asked for the PR, if anything.',
    inputSchema: {
      type: 'object',
      properties: { sessionId: { type: 'string' }, instructions: { type: 'string' } },
      required: ['sessionId'],
    },
  },
  {
    name: 'open_project',
    description:
      'Opens a Kode project on a repo: a repo folder Kode scans, GitHub owner/name (cloned when there is no clone), or a clone URL. ' +
      'datapack: new (a new Meko datapack, the default), none, or a datapack id.',
    inputSchema: {
      type: 'object',
      properties: {
        repo: { type: 'string' },
        name: { type: 'string', description: "The project's name. Default: the repo's." },
        datapack: { type: 'string' },
      },
      required: ['repo'],
    },
  },
  {
    name: 'review_report',
    readOnly: true,
    description:
      "One pass of Kode's code review reports what it found. Only for a review step whose file names this tool: " +
      'step is the step id the file gives, report the JSON its schema asks for. An error says what to fix; call again.',
    inputSchema: { type: 'object', properties: { step: { type: 'string' }, report: { type: 'object' } }, required: ['step', 'report'] },
  },
  {
    name: 'review_score',
    readOnly: true,
    description:
      "A scorer of Kode's code review reports its verdict on one finding. Only for a step whose file names this tool: " +
      'step is the step id the file gives, score the JSON its schema asks for. An error says what to fix; call again.',
    inputSchema: { type: 'object', properties: { step: { type: 'string' }, score: { type: 'object' } }, required: ['step', 'score'] },
  },
  {
    name: 'review_outcome',
    readOnly: true,
    description:
      "Tells Kode what you did with each finding of its code review, by the key Kode gave it ([1:2]: key 1:2): " +
      'status fixed or declined, and note: what you changed, or why the finding is wrong.',
    inputSchema: {
      type: 'object',
      properties: {
        outcomes: {
          type: 'array',
          items: {
            type: 'object',
            properties: { key: { type: 'string' }, status: { type: 'string', enum: ['fixed', 'declined'] }, note: { type: 'string' } },
            required: ['key', 'status', 'note'],
          },
        },
      },
      required: ['outcomes'],
    },
  },
];

/** The tools a harness may run without asking, by their short names. */
const READ_ONLY = TOOL_DEFS.filter((t) => t.readOnly).map((t) => t.name);

/** Claude Code's names for them (`permissions.allow`). */
export function claudeAllowedTools(): string[] {
  return READ_ONLY.map((t) => `mcp__${TOOLS_SERVER}__${t}`);
}
