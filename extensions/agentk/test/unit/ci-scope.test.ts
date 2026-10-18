import { spawnSync } from 'node:child_process';
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

// The repo root's scripts/ci-scope.sh decides which components' jobs a PR runs in
// .github/workflows/ci.yml, and the CI gate job is the one check merging requires.
const SCOPE = fileURLToPath(new URL('../../../../scripts/ci-scope.sh', import.meta.url));
const WORKFLOWS = fileURLToPath(new URL('../../../../.github/workflows/', import.meta.url));
const TESTS = fileURLToPath(new URL('..', import.meta.url));

const scope = (input: string, ...args: string[]) => spawnSync('sh', [SCOPE, ...args], { input, encoding: 'utf8' });
const run = (...paths: string[]) => {
  const r = scope(paths.join('\n'));
  expect(r.status, r.stderr).toBe(0);
  return r.stdout;
};
const out = (agentk: boolean, backend: boolean) => `agentk=${agentk}\nbackend=${backend}\n`;

describe('ci-scope.sh', () => {
  it.each([
    ['extensions/agentk/src/server/core.ts', true, false],
    ['extensions/agentk/package-lock.json', true, false],
    ['scripts/check-title.sh', true, false],
    ['.github/dependabot.yml', true, false],
    ['.github/workflows/pr-title.yml', true, false],
    ['.github/workflows/release.yml', true, false],
    ['backend/cmd/meko-local/main.go', false, true],
    ['backend/AGENTS.md', false, true],
    ['.github/workflows/ci.yml', true, true],
    ['scripts/ci-scope.sh', true, true],
    ['AGENTS.md', false, false],
    ['README.md', false, false],
    ['.claude/skills/commit/SKILL.md', false, false],
    ['.gitignore', true, true],
    ['.gitattributes', true, true],
    ['LICENSE', true, true],
    ['docs/new.md', true, true],
    ['extensions/other/index.ts', true, true],
  ])('%s runs agentk=%s backend=%s', (path, agentk, backend) => {
    expect(run(path)).toBe(out(agentk, backend));
  });

  it('runs each component any path needs', () => {
    expect(run('AGENTS.md', 'backend/go.mod')).toBe(out(false, true));
    expect(run('backend/go.mod', 'extensions/agentk/src/a.ts')).toBe(out(true, true));
    expect(run('README.md', '.claude/x', 'extensions/agentk/README.md')).toBe(out(true, false));
  });

  it('runs nothing for no paths, and reads a last line without a newline', () => {
    expect(run()).toBe(out(false, false));
    expect(scope('\n\n').stdout).toBe(out(false, false));
    expect(scope('AGENTS.md\nbackend/go.mod').stdout).toBe(out(false, true));
  });

  it('runs everything with --all', () => {
    expect(scope('', '--all').stdout).toBe(out(true, true));
  });

  it('exits 2 on bad usage', () => {
    expect(scope('', 'backend/go.mod').status).toBe(2);
    expect(scope('', '--all', 'x').status).toBe(2);
  });

  // A root file AgentK's tests read must run them when it changes.
  it('runs AgentK for every root file its tests read', () => {
    const files = readdirSync(TESTS, { recursive: true, encoding: 'utf8' }).filter((f) => f.endsWith('.ts'));
    const reads = new Set<string>();
    for (const f of files) {
      for (const m of readFileSync(join(TESTS, f), 'utf8').matchAll(/['`]\.\.\/\.\.\/\.\.\/\.\.\/([^'`$]*)/g)) {
        // A template like `.github/workflows/${name}` stands for any file in that folder.
        reads.add(m[1]!.endsWith('/') || m[1] === '' ? `${m[1]}any-file` : m[1]!);
      }
    }
    expect(reads).toContain('scripts/check-title.sh');
    expect(reads).toContain('.github/dependabot.yml');
    for (const path of reads) expect(run(path), path).toMatch(/^agentk=true$/m);
  });
});

describe('the CI workflow', () => {
  const ci = readFileSync(join(WORKFLOWS, 'ci.yml'), 'utf8');
  /** The jobs, by key, each with its lines. */
  const jobs = new Map(
    [...ci.slice(ci.indexOf('\njobs:\n')).matchAll(/^ {2}([\w-]+):\n((?: {4}.*\n|\s*\n|\s*#.*\n)*)/gm)].map((m) => [m[1]!, m[2]!]),
  );

  it('gates every job, and the gate always runs', () => {
    const gate = jobs.get('gate')!;
    expect(gate).toContain('    name: CI gate\n');
    expect(gate).toContain('    if: always()\n');
    const needs = /^ {4}needs: \[([^\]]*)\]$/m.exec(gate)![1]!.split(',').map((s) => s.trim());
    expect(needs.sort()).toEqual([...jobs.keys()].filter((k) => k !== 'gate').sort());
  });

  it('runs each component only when the change needs it', () => {
    expect([...jobs.keys()]).toEqual(expect.arrayContaining(['changes', 'agentk', 'backend']));
    for (const c of ['agentk', 'backend']) {
      expect(jobs.get('changes')).toContain(`      ${c}: \${{ steps.scope.outputs.${c} }}\n`);
      expect(jobs.get(c)).toContain('    needs: changes\n');
      expect(jobs.get(c)).toContain(`    if: needs.changes.outputs.${c} == 'true'\n`);
    }
    expect(jobs.get('changes')).toContain('sh scripts/ci-scope.sh');
    // Run steps have no pipefail: a failed diff piped into the script would skip everything.
    expect(jobs.get('changes')).not.toMatch(/\|\s*sh scripts\/ci-scope\.sh/);
  });

  it('always starts, and holds every component', () => {
    expect(ci).not.toMatch(/^\s*paths(-ignore)?:/m);
    expect(existsSync(join(WORKFLOWS, 'backend.yml'))).toBe(false);
  });
});
