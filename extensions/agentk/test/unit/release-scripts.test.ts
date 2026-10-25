import { execFileSync, spawnSync } from 'node:child_process';
import { chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';

// The repo root's scripts/release/*.sh release AgentK and Meko from one history: each product has
// its own tags, versions, notes and pruning (--product, scripts/release/product.sh). The rules
// about which types release are tested in check-title.test.ts.
const DIR = fileURLToPath(new URL('../../../../scripts/release/', import.meta.url));

let root: string;
let repo: string;
beforeEach(() => {
  root = tempDir('akl-release-');
  repo = gitRepo(join(root, 'repo'));
});
afterEach(() => rmSync(root, { recursive: true, force: true }));

const git = (...args: string[]) => execFileSync('git', args, { cwd: repo, encoding: 'utf8' });
const commit = (msg: string) => git('commit', '-q', '--allow-empty', '-m', msg);
const run = (script: string, args: string[], env: Record<string, string> = {}) =>
  spawnSync('sh', [join(DIR, script), ...args], { cwd: repo, encoding: 'utf8', env: { ...process.env, ...env } });

describe('next-version.sh', () => {
  it.each([
    [['--product', 'agentk'], '0.20260924.3'],
    [[], '0.20260924.3'],
    [['--product=meko'], '0.20260924.1'],
  ])('counts only the product’s own tags that day, AgentK’s of both prefixes (%j)', (args, want) => {
    // The day AgentK switched from v… to agentk-v… tags: a version is never used twice.
    for (const t of ['v0.20260923.7', 'v0.20260924.0', 'v0.20260924.1', 'agentk-v0.20260924.2', 'meko-v0.20260924.0']) git('tag', t);
    const r = run('next-version.sh', args, { RELEASE_DATE: '20260924' });
    expect(r.stderr).toBe('');
    expect(r.stdout.trim()).toBe(want);
  });

  it('refuses an unknown product', () => {
    const r = run('next-version.sh', ['--product', 'web']);
    expect(r.status).toBe(2);
    expect(r.stderr).toContain('unknown product: web');
  });
});

describe('release-notes.sh', () => {
  beforeEach(() => {
    commit('feat(agentk): first');
    git('tag', 'v0.20260101.0');
    commit('feat(meko): first');
    git('tag', 'meko-v0.20260101.0');
  });

  it('gives AgentK every releasing subject but Meko’s and Presenter’s, since AgentK’s last tag of either prefix', () => {
    commit('fix(agentk): before the switch');
    git('tag', 'agentk-v0.20260102.0');
    commit('fix(agentk/web): stop clipping the rail');
    commit('fix(repo): a release script');
    commit('feat(meko/cli)!: a Meko command');
    commit('feat(presenter/web): a Presenter page');
    commit('fix(meko): a Meko fix');
    commit('feat(meko,presenter): a Meko and Presenter change');
    const r = run('release-notes.sh', ['--product', 'agentk']);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('- agentk/web: stop clipping the rail');
    expect(r.stdout).toContain('- repo: a release script');
    expect(r.stdout).not.toMatch(/meko|presenter|first|before the switch|Breaking/);
  });

  it('gives Meko only its own subjects, since Meko’s last tag, and exits 3 with none', () => {
    commit('feat(agentk): an AgentK feature');
    expect(run('release-notes.sh', ['--product', 'meko']).status).toBe(3);
    commit('fix(meko/engine)!: reload the model');
    const r = run('release-notes.sh', ['--product', 'meko']);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Breaking changes\n\n- fix(meko/engine)!: reload the model\n');
    expect(r.stdout).toContain('### Fixes\n\n- meko/engine: reload the model');
    expect(r.stdout).not.toMatch(/AgentK feature|first/);
  });

  it('releases a subject that names both components on both trains', () => {
    commit('feat(agentk/meko,meko/cli): move the home into Meko');
    commit('fix(repo,meko): a shared script');
    commit('perf(meko/engine,agentk): load faster');
    // Lookalikes of meko, alone and in a list, aren't Meko's.
    commit('fix(mekoish): an old free-form component');
    commit('fix(agentk,mekoish): a lookalike last in a list');
    commit('fix(mekoish/area,repo): a lookalike first in a list');
    for (const product of ['agentk', 'meko']) {
      const r = run('release-notes.sh', ['--product', product]);
      expect(r.status, product).toBe(0);
      expect(r.stdout, product).toContain('### Features\n\n- agentk/meko,meko/cli: move the home into Meko');
      expect(r.stdout, product).toContain('- repo,meko: a shared script');
      expect(r.stdout, product).toContain('### Performance\n\n- meko/engine,agentk: load faster');
    }
    expect(run('release-notes.sh', ['--product', 'meko']).stdout).not.toContain('mekoish');
    const agentk = run('release-notes.sh', ['--product', 'agentk']).stdout;
    for (const s of ['- mekoish: ', '- agentk,mekoish: ', '- mekoish/area,repo: ']) expect(agentk).toContain(s);
  });
});

describe('prune-releases.sh', () => {
  const hasJq = spawnSync('jq', ['--version']).status === 0;

  it.skipIf(!hasJq)('deletes only the product’s own releases beyond KEEP in each lane', () => {
    // A stand-in gh that answers `release list` from canned JSON through its --jq filter.
    const bin = join(root, 'bin');
    mkdirSync(bin);
    writeFileSync(join(bin, 'gh'), '#!/bin/sh\n[ "$1 $2" = "release list" ] || exit 1\nwhile [ $# -gt 0 ]; do [ "$1" = --jq ] && q=$2; shift; done\nexec jq -r "$q" "$RELEASES"\n');
    chmodSync(join(bin, 'gh'), 0o755);
    const releases = join(root, 'releases.json');
    const rel = (tagName: string, day: number, isPrerelease = false) => ({ tagName, isPrerelease, createdAt: `2026-01-0${day}T00:00:00Z` });
    writeFileSync(releases, JSON.stringify([
      rel('v0.20260101.0', 1), rel('meko-v0.20260101.0', 2), rel('v0.20260102.0', 3),
      rel('meko-v0.20260102.0', 4), rel('agentk-v0.20260103.0-beta.1', 5, true), rel('agentk-v0.20260103.0', 6),
      rel('agentk-v0.20260104.0', 7),
    ]));
    const prune = (product: string, keep: string) => run('prune-releases.sh', ['--product', product, '--dry-run'], {
      PATH: `${bin}:${process.env.PATH}`, RELEASES: releases, GITHUB_REPOSITORY: 'o/r', KEEP: keep,
    }).stdout;
    // The v… releases (before the switch) are a lane of their own, which stops growing.
    expect(prune('agentk', '1')).toBe('would delete agentk-v0.20260103.0\nwould delete v0.20260101.0\n');
    expect(prune('meko', '1')).toBe('would delete meko-v0.20260101.0\n');
    // The newest v… release, which older installs update through, is never deleted.
    expect(prune('agentk', '0')).toBe([
      'agentk-v0.20260104.0', 'agentk-v0.20260103.0', 'agentk-v0.20260103.0-beta.1', 'v0.20260101.0',
    ].map((t) => `would delete ${t}\n`).join(''));
  });
});

describe('agentk-release.yml’s hand-run rebuild', () => {
  // The plan step's own shell, cut out of the workflow: its run block, dedented.
  const plan = (() => {
    const lines = readFileSync(fileURLToPath(new URL('../../../../.github/workflows/agentk-release.yml', import.meta.url)), 'utf8').split('\n');
    const start = lines.findIndex((l) => /^\s+- id: p$/.test(l));
    const run = lines.findIndex((l, i) => i > start && /^\s+run: \|$/.test(l));
    const indent = /^\s*/.exec(lines[run + 1]!)![0];
    const body: string[] = [];
    for (const l of lines.slice(run + 1)) {
      if (l.trim() && !l.startsWith(indent)) break;
      body.push(l.slice(indent.length));
    }
    return body.join('\n');
  })();
  const rebuild = (tag: string) => {
    const out = join(root, 'out');
    writeFileSync(out, '');
    const r = spawnSync('sh', ['-c', plan], { cwd: repo, encoding: 'utf8', env: { ...process.env, DISPATCH_TAG: tag, GITHUB_OUTPUT: out } });
    return { status: r.status, stdout: r.stdout, output: readFileSync(out, 'utf8') };
  };

  beforeEach(() => {
    git('tag', 'v0.20261001.0'); // from before the move to extensions/agentk
    mkdirSync(join(repo, 'extensions/agentk'), { recursive: true });
    writeFileSync(join(repo, 'extensions/agentk/package.json'), '{}\n');
    git('add', '.');
    commit('chore(repo): move AgentK');
    for (const t of ['v0.20261022.2', 'agentk-v0.20261023.0', 'meko-v0.20261023.0']) git('tag', t);
  });

  it.each([
    ['agentk-v0.20261023.0', '0.20261023.0'],
    ['v0.20261022.2', '0.20261022.2'],
  ])('rebuilds an AgentK tag of either prefix (%s)', (tag, version) => {
    const r = rebuild(tag);
    expect(r.status).toBe(0);
    expect(r.output).toBe(`release=true\nrebuild=true\ntag=${tag}\nversion=${version}\nref=${tag}\n`);
  });

  it.each([
    ['meko-v0.20261023.0', "isn't an AgentK release tag"],
    ['agentk-v0.20261099.0', 'no such tag'],
    ['v0.20261001.0', 'from before AgentK moved'],
  ])('refuses %s', (tag, why) => {
    const r = rebuild(tag);
    expect(r.status).toBe(1);
    expect(r.stdout).toContain(why);
    expect(r.output).toBe('');
  });
});
