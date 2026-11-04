import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG } from '../../src/server/config.js';
import type { RepoVisibility } from '../../src/server/github.js';
import { kiroHarness } from '../../src/server/harness/kiro.js';
import type { LaunchInput } from '../../src/server/harness/types.js';
import { HeadTrust, kiroWorkspaceConfig, prHead, repoConfigTrusted } from '../../src/server/headtrust.js';
import type { Paths } from '../../src/server/paths.js';
import type { SessionRow } from '../../src/server/store.js';
import { tempDir } from '../helpers.js';

const row = (over: Partial<SessionRow>) =>
  ({ kind: 'plan', pr_repo: null, pr_url: null, pr_number: null, branch_reused: 0, untrusted: null, ...over }) as SessionRow;

describe("whose code a session's PR head is", () => {
  it.each([
    ['a review', { kind: 'review-pr', pr_repo: 'acme/demo' }, { repo: 'acme/demo' }],
    ['a review whose repo is only in its link', { kind: 'review-pr', pr_url: 'https://github.com/acme/demo/pull/7' }, { repo: 'acme/demo' }],
    ['a review that names no repo', { kind: 'review-pr' }, { repo: null }],
    ['a task on a PR', { branch_reused: 1, pr_number: 5, pr_url: 'https://github.com/acme/demo/pull/5' }, { repo: 'acme/demo' }],
    ['a task on its own branch that opened a PR', { pr_number: 5, pr_url: 'https://github.com/acme/demo/pull/5' }, null],
    ['a task on an existing branch with no PR', { branch_reused: 1 }, null],
  ] as const)('%s', (_, over, want) => {
    expect(prHead(row(over as Partial<SessionRow>))).toEqual(want);
  });

  it("uses the repo's config only once a PR head is known to be trusted", () => {
    expect(repoConfigTrusted(row({}))).toBe(true);
    expect(repoConfigTrusted(row({ kind: 'review-pr', pr_repo: 'acme/demo' }))).toBe(false);
    expect(repoConfigTrusted(row({ kind: 'review-pr', pr_repo: 'acme/demo', untrusted: 1 }))).toBe(false);
    expect(repoConfigTrusted(row({ kind: 'review-pr', pr_repo: 'acme/demo', untrusted: 0 }))).toBe(true);
    // A task on an existing branch that took its PR after it was checked (with no PR head: NULL) isn't trusted until checked again.
    expect(repoConfigTrusted(row({ branch_reused: 1, pr_number: 5, pr_url: 'https://github.com/acme/demo/pull/5', untrusted: null }))).toBe(false);
  });
});

describe("trusting a PR head by its repo's visibility", () => {
  const review = row({ kind: 'review-pr', pr_repo: 'acme/demo' });

  it.each([
    ['public', 'acme/demo is public'],
    ['private', null],
    ['internal', null],
  ] as const)('a %s repo', async (visibility, why) => {
    expect(await new HeadTrust(async () => visibility).untrusted(review)).toBe(why);
  });

  it("doesn't trust a repo gh can't read, reads it again next time, and keeps an answer for a while", async () => {
    const answers: (RepoVisibility | Error)[] = [new Error('gh: not logged in'), 'private', 'public'];
    let reads = 0;
    const trust = new HeadTrust(async () => {
      reads++;
      const a = answers.shift()!;
      if (a instanceof Error) throw a;
      return a;
    });
    expect(await trust.untrusted(review)).toBe("GitHub didn't say whether acme/demo is public");
    expect(await trust.untrusted(review)).toBeNull();
    expect(await trust.untrusted(review)).toBeNull();
    expect(reads).toBe(2);
    // Once it's stale, a repo made public since is read as such.
    expect(await trust.visibility('acme/demo', Date.now() + 11 * 60 * 1000)).toBe('public');
    // Nothing to read for a session on its own branch, or a review with no repo.
    expect(await trust.untrusted(row({}))).toBeNull();
    expect(await trust.untrusted(row({ kind: 'review-pr' }))).toBe("AgentK doesn't know which repo this PR is on");
    expect(reads).toBe(3);
  });
});

describe("Kiro config a PR brings", () => {
  it("finds a workspace agentk agent, by file or by name, and workspace MCP servers; Kiro won't start an untrusted session over them", () => {
    const wt = tempDir('akl-headtrust-');
    mkdirSync(join(wt, '.kiro', 'agents'), { recursive: true });
    writeFileSync(join(wt, '.kiro', 'agents', 'helper.json'), JSON.stringify({ name: 'helper' }));
    expect(kiroWorkspaceConfig(wt, 'agentk')).toEqual([]);
    writeFileSync(join(wt, '.kiro', 'agents', 'mine.json'), JSON.stringify({ name: 'agentk' }));
    mkdirSync(join(wt, '.kiro', 'settings'), { recursive: true });
    writeFileSync(join(wt, '.kiro', 'settings', 'mcp.json'), '{}');
    expect(kiroWorkspaceConfig(wt, 'agentk')).toEqual(['.kiro/agents/mine.json', '.kiro/settings/mcp.json']);

    const kiro = kiroHarness({ log: { info() {}, warn() {}, error() {} } as never, changed: () => {} });
    const launch = (untrusted: boolean): LaunchInput => ({
      row: row({ id: 's1', worktree_path: wt }), resume: false, prompt: null, model: null, systemPrompt: '', addDirs: [], modDir: null, planMode: false, env: {}, unsetEnv: [], untrusted, tools: false,
    });
    expect(() => kiro.launcher({} as Paths, DEFAULT_CONFIG, launch(true))).toThrow(
      "Kiro would run this PR's own config (the agent in .kiro/agents/mine.json in place of AgentK's, and the MCP servers in .kiro/settings/mcp.json), "
        + "and anyone can have written this PR's code. Review it on Claude Code instead, which can leave the repo's config out.");
    expect(kiro.launcher({} as Paths, DEFAULT_CONFIG, launch(false)).args).toEqual(['chat', '--agent', 'agentk']);
  });
});
