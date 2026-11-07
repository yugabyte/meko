import { execFileSync, spawnSync } from 'node:child_process';
import { createHash, createPublicKey, generateKeyPairSync, verify, type KeyObject } from 'node:crypto';
import { chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';

// The repo root's scripts/release/*.sh release AgentK, Meko and Presenter from one history: each
// product has its own tags, versions, notes and pruning (--product, scripts/release/product.sh). The rules
// about which types release are tested in check-title.test.ts.
const DIR = fileURLToPath(new URL('../../../../../scripts/release/', import.meta.url));

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
    [['--product', 'present'], '0.20260924.0'],
  ])('counts only the product’s own tags that day, AgentK’s of both prefixes (%j)', (args, want) => {
    // The day AgentK switched from v… to agentk-v… tags: a version is never used twice.
    for (const t of ['v0.20260923.7', 'v0.20260924.0', 'v0.20260924.1', 'agentk-v0.20260924.2', 'meko-v0.20260924.0', 'present-v0.20260923.0']) git('tag', t);
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
    commit('feat(present/web): a Presenter page');
    commit('fix(meko): a Meko fix');
    commit('feat(meko,present): a Meko and Presenter change');
    commit('fix(presenter/web): a Presenter fix under its old component');
    const r = run('release-notes.sh', ['--product', 'agentk']);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('- agentk/web: stop clipping the rail');
    expect(r.stdout).toContain('- repo: a release script');
    expect(r.stdout).not.toMatch(/meko|present|Presenter|first|before the switch|Breaking/);
  });

  it('gives Meko only its own subjects, since Meko’s last tag, and exits 3 with none', () => {
    commit('feat(agentk): an AgentK feature');
    expect(run('release-notes.sh', ['--product', 'meko']).status).toBe(3);
    commit('fix(meko/engine)!: reload the model');
    const r = run('release-notes.sh', ['--product', 'meko']);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Breaking changes\n\n- fix(meko/engine)!: reload the model\n');
    expect(r.stdout).not.toContain('### Fixes');
    expect(r.stdout).not.toMatch(/AgentK feature|first/);
  });

  it('gives Presenter only the subjects naming present, alone or in a list, and from the first commit before its first tag', () => {
    commit('feat(agentk): an AgentK feature');
    commit('fix(meko): a Meko fix');
    commit('feat(presentish): a lookalike');
    expect(run('release-notes.sh', ['--product', 'present']).status).toBe(3);
    commit('feat(present/web): a Presenter page');
    commit('feat(present,meko): install Presenter from its releases');
    commit('fix(agentk,present/server): a shared fix');
    const r = run('release-notes.sh', ['--product', 'present']);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Features\n\n- present,meko: install Presenter from its releases');
    expect(r.stdout).toContain('- present/web: a Presenter page');
    expect(r.stdout).toContain('### Fixes\n\n- agentk,present/server: a shared fix');
    expect(r.stdout).not.toMatch(/AgentK feature|Meko fix|lookalike|first/);
    // Presenter's subjects stay out of AgentK's notes; the one naming meko is Meko's too.
    expect(run('release-notes.sh', ['--product', 'agentk']).stdout).not.toMatch(/Presenter page|its releases/);
    expect(run('release-notes.sh', ['--product', 'meko']).stdout).toContain('- present,meko: install Presenter from its releases');
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
      rel('agentk-v0.20260104.0', 7), rel('present-v0.20260104.0', 8), rel('present-v0.20260105.0', 9),
    ]));
    const prune = (product: string, keep: string) => run('prune-releases.sh', ['--product', product, '--dry-run'], {
      PATH: `${bin}:${process.env.PATH}`, RELEASES: releases, GITHUB_REPOSITORY: 'o/r', KEEP: keep,
    }).stdout;
    // The v… releases (before the switch) are a lane of their own, which stops growing.
    expect(prune('agentk', '1')).toBe('would delete agentk-v0.20260103.0\nwould delete v0.20260101.0\n');
    expect(prune('meko', '1')).toBe('would delete meko-v0.20260101.0\n');
    expect(prune('present', '1')).toBe('would delete present-v0.20260104.0\n');
    // The newest v… release, which older installs update through, is never deleted.
    expect(prune('agentk', '0')).toBe([
      'agentk-v0.20260104.0', 'agentk-v0.20260103.0', 'agentk-v0.20260103.0-beta.1', 'v0.20260101.0',
    ].map((t) => `would delete ${t}\n`).join(''));
  });
});

// A release workflow's plan step's own shell, cut out of the workflow: its run block, dedented.
function planStep(workflow: string): string {
  const lines = readFileSync(fileURLToPath(new URL(`../../../../../.github/workflows/${workflow}`, import.meta.url)), 'utf8').split('\n');
  const start = lines.findIndex((l) => /^\s+- id: p$/.test(l));
  const run = lines.findIndex((l, i) => i > start && /^\s+run: \|$/.test(l));
  const indent = /^\s*/.exec(lines[run + 1]!)![0];
  const body: string[] = [];
  for (const l of lines.slice(run + 1)) {
    if (l.trim() && !l.startsWith(indent)) break;
    body.push(l.slice(indent.length));
  }
  return body.join('\n');
}

describe('agentk-release.yml’s hand-run rebuild', () => {
  const plan = planStep('agentk-release.yml');
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
    git('tag', 'agentk-v0.20261020.0'); // from before the move to src/extensions/agentk
    mkdirSync(join(repo, 'src'));
    git('mv', 'extensions', 'src/extensions');
    commit('chore(repo): move the source under src');
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
    ['agentk-v0.20261020.0', 'from before AgentK moved to src/extensions/agentk'],
  ])('refuses %s', (tag, why) => {
    const r = rebuild(tag);
    expect(r.status).toBe(1);
    expect(r.stdout).toContain(why);
    expect(r.output).toBe('');
  });
});

describe('present-release.yml’s hand-run rebuild', () => {
  const plan = planStep('present-release.yml');
  const rebuild = (tag: string) => {
    const out = join(root, 'out');
    writeFileSync(out, '');
    const r = spawnSync('sh', ['-c', plan], { cwd: repo, encoding: 'utf8', env: { ...process.env, DISPATCH_TAG: tag, GITHUB_OUTPUT: out } });
    return { status: r.status, stdout: r.stdout, output: readFileSync(out, 'utf8') };
  };

  beforeEach(() => {
    commit('feat(present): first');
    git('tag', 'present-v0.20261020.0'); // from before the move to src/extensions/present
    mkdirSync(join(repo, 'src/extensions/present'), { recursive: true });
    writeFileSync(join(repo, 'src/extensions/present/package.json'), '{}\n');
    git('add', '.');
    commit('chore(repo): move the source under src');
    for (const t of ['present-v0.20261024.0', 'agentk-v0.20261024.0', 'meko-v0.20261024.0']) git('tag', t);
  });

  it('rebuilds a Presenter tag', () => {
    const r = rebuild('present-v0.20261024.0');
    expect(r.status).toBe(0);
    expect(r.output).toBe('release=true\nrebuild=true\ntag=present-v0.20261024.0\nversion=0.20261024.0\nref=present-v0.20261024.0\n');
  });

  it.each([
    ['agentk-v0.20261024.0', "isn't a Presenter release tag"],
    ['meko-v0.20261024.0', "isn't a Presenter release tag"],
    ['present-v0.20261099.0', 'no such tag'],
    ['present-v0.20261020.0', 'from before Presenter moved to src/extensions/present'],
  ])('refuses %s', (tag, why) => {
    const r = rebuild(tag);
    expect(r.status).toBe(1);
    expect(r.stdout).toContain(why);
    expect(r.output).toBe('');
  });
});

describe('meko-release.yml’s hand-run rebuild', () => {
  const plan = planStep('meko-release.yml');
  const rebuild = (tag: string) => {
    const out = join(root, 'out');
    writeFileSync(out, '');
    const r = spawnSync('sh', ['-c', plan], { cwd: repo, encoding: 'utf8', env: { ...process.env, DISPATCH_TAG: tag, GITHUB_OUTPUT: out } });
    return { status: r.status, stdout: r.stdout, output: readFileSync(out, 'utf8') };
  };

  beforeEach(() => {
    mkdirSync(join(repo, 'backend'));
    writeFileSync(join(repo, 'backend/go.mod'), 'module x\n');
    git('add', '.');
    commit('feat(meko): first');
    git('tag', 'meko-v0.20261020.0'); // from before the move to src/meko
    mkdirSync(join(repo, 'src'));
    git('mv', 'backend', 'src/meko');
    commit('chore(repo): move the source under src');
    for (const t of ['meko-v0.20261024.0', 'agentk-v0.20261024.0', 'present-v0.20261024.0']) git('tag', t);
  });

  it('rebuilds a Meko tag', () => {
    const r = rebuild('meko-v0.20261024.0');
    expect(r.status).toBe(0);
    expect(r.output).toBe('release=true\nrebuild=true\ntag=meko-v0.20261024.0\nversion=0.20261024.0\nref=meko-v0.20261024.0\n');
  });

  it.each([
    ['agentk-v0.20261024.0', "isn't a Meko release tag"],
    ['present-v0.20261024.0', "isn't a Meko release tag"],
    ['meko-v0.20261099.0', 'no such tag'],
    ['meko-v0.20261020.0', 'from before Meko moved to src/meko'],
  ])('refuses %s', (tag, why) => {
    const r = rebuild(tag);
    expect(r.status).toBe(1);
    expect(r.stdout).toContain(why);
    expect(r.output).toBe('');
  });
});

// The release workflows' jobs, each a list of its steps' lines: a job starts at `  <name>:`, a
// step at `      - `.
function jobs(name: string): string[][][] {
  const out: string[][][] = [];
  let step: string[] | undefined;
  for (const l of readFileSync(fileURLToPath(new URL(`../../../../../.github/workflows/${name}`, import.meta.url)), 'utf8').split('\n')) {
    if (/^ {2}\S+:$/.test(l)) out.push([]);
    else if (l.startsWith('      - ') && out.length) out.at(-1)!.push((step = [l]));
    else if (step && (!l.trim() || l.startsWith('        '))) step.push(l);
    else step = undefined;
  }
  return out;
}
const code = (step: string[]) => step.filter((l) => !/^\s*#/.test(l)).join('\n');

// On 2026-10-28 three AgentK releases broke on downloads: a dnf mirror at 110 kB/s, node-gyp
// stuck fetching Node's headers, and a broken HTTP/2 stream from nodejs.org.
describe.each(['agentk-release.yml', 'meko-release.yml', 'present-release.yml'])('%s’s downloads', (name) => {
  const all = jobs(name);

  it('give every step that downloads a timeout of its own', () => {
    const downloading = all.flat().filter((s) => /\b(curl|fetch-node\.sh|dnf install|apt-get|npm ci|make (test|memory_engine|web_ui))\b/.test(code(s)));
    expect(downloading.length).toBeGreaterThan(0);
    for (const s of downloading) expect(s.join('\n'), s[0]).toMatch(/^\s+(- )?timeout-minutes: \d+$/m);
  });

  // A hand-run rebuild builds an older tag, which may not have the script: it runs from the
  // workflow's own commit, and the tag is checked out after it.
  it('get Node only through fetch-node.sh, from the workflow’s own commit', () => {
    expect(all.flat().filter((s) => /\bcurl\b/.test(code(s)))).toEqual([]);
    const fetching = all.filter((job) => job.some((s) => /fetch-node\.sh/.test(code(s))));
    expect(fetching.length).toBeGreaterThan(0);
    for (const job of fetching) {
      const checkouts = job.map((s, i) => [i, code(s)] as const).filter(([, c]) => /uses: actions\/checkout@/.test(c));
      const fetchAt = job.findIndex((s) => /fetch-node\.sh/.test(code(s)));
      const before = checkouts.filter(([i]) => i < fetchAt);
      expect(before.map(([, c]) => /ref: \$\{\{ github\.sha \}\}/.test(c))).toEqual([true]);
      for (const [, c] of checkouts.filter(([i]) => i > fetchAt)) {
        expect(c).toMatch(/if: needs\.plan\.outputs\.rebuild == 'true'[\s\S]*ref: \$\{\{ needs\.plan\.outputs\.ref \}\}/);
      }
    }
  });
});

describe('fetch-node.sh', () => {
  // A fake nodejs.org: curl copies the file named by the URL from srv/, after failing (as a
  // broken HTTP/2 stream does, exit 92) its first `failures` times. sleep only logs.
  const fetchNode = (platforms: string[], failures = 0) => {
    const bin = join(root, 'bin');
    const srv = join(root, 'srv');
    mkdirSync(bin, { recursive: true });
    writeFileSync(join(bin, 'curl'), `#!/bin/sh
n=$(cat "${root}/tries" 2>/dev/null || echo 0)
echo $((n + 1)) > "${root}/tries"
[ "$n" -ge ${failures} ] || exit 92
for a; do url=$a; done
echo "$url" >> "${root}/urls"
cp "${srv}/\${url##*/}" .
`);
    writeFileSync(join(bin, 'sleep'), `#!/bin/sh\necho "$1" >> "${root}/sleeps"\n`);
    chmodSync(join(bin, 'curl'), 0o755);
    chmodSync(join(bin, 'sleep'), 0o755);
    const r = spawnSync('sh', [join(DIR, 'fetch-node.sh'), '22.18.0', join(root, 'out'), ...platforms], {
      cwd: root, encoding: 'utf8', env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
    });
    const read = (f: string) => { try { return readFileSync(join(root, f), 'utf8'); } catch { return ''; } };
    return { ...r, tries: Number(read('tries')), urls: read('urls'), sleeps: read('sleeps') };
  };
  // Serves a real tarball for each platform (.tar.xz for linux, .tar.gz for darwin) and their sums.
  const serve = (platforms: string[], sums?: string) => {
    const srv = join(root, 'srv');
    let all = '';
    for (const p of platforms) {
      const dist = `node-v22.18.0-${p}`;
      const file = `${dist}.tar.${p.startsWith('linux') ? 'xz' : 'gz'}`;
      mkdirSync(join(srv, dist, 'bin'), { recursive: true });
      writeFileSync(join(srv, dist, 'bin', 'node'), '#!/bin/sh\n');
      execFileSync('tar', [p.startsWith('linux') ? '-cJf' : '-czf', join(srv, file), '-C', srv, dist]);
      all += execFileSync('sh', ['-c', `cd "${srv}" && (sha256sum "${file}" 2>/dev/null || shasum -a 256 "${file}")`], { encoding: 'utf8' });
    }
    writeFileSync(join(srv, 'SHASUMS256.txt'), sums ?? `${'0'.repeat(64)}  node-v22.18.0-other.tar.gz\n${all}`);
  };

  it('downloads SHASUMS256.txt once, checks and unpacks each platform, and prints their folders', () => {
    serve(['linux-x64', 'darwin-arm64']);
    const r = fetchNode(['linux-x64', 'darwin-arm64']);
    expect(r.status).toBe(0);
    expect(r.stdout).toBe(['linux-x64', 'darwin-arm64'].map((p) => `${join(root, 'out', `node-v22.18.0-${p}`)}\n`).join(''));
    for (const p of ['linux-x64', 'darwin-arm64']) {
      expect(readFileSync(join(root, 'out', `node-v22.18.0-${p}`, 'bin', 'node'), 'utf8')).toBe('#!/bin/sh\n');
    }
    expect(r.urls).toBe([
      'SHASUMS256.txt', 'node-v22.18.0-linux-x64.tar.xz', 'node-v22.18.0-darwin-arm64.tar.gz',
    ].map((f) => `https://nodejs.org/dist/v22.18.0/${f}\n`).join(''));
  });

  it.each([
    [2, 0, 4, '10\n10\n'], // two failures, then SHASUMS256.txt and the tarball
    [5, 1, 3, '10\n10\n'], // gives up after three tries, with no pause after the last
  ])('retries any failure, %i times failing', (failures, status, tries, sleeps) => {
    serve(['linux-x64']);
    const r = fetchNode(['linux-x64'], failures);
    expect({ status: r.status, tries: r.tries, sleeps: r.sleeps }).toEqual({ status, tries, sleeps });
    expect(r.stderr).toContain('Downloading SHASUMS256.txt failed (try 2 of 3)');
  });

  it.each([
    ['a wrong checksum', `${'0'.repeat(64)}  node-v22.18.0-linux-x64.tar.xz\n`],
    ['no checksum', `${'0'.repeat(64)}  node-v22.18.0-linux-arm64.tar.xz\n`],
  ])('refuses a tarball with %s', (_, sums) => {
    serve(['linux-x64'], sums);
    const r = fetchNode(['linux-x64']);
    expect(r.status).not.toBe(0);
    expect(r.stdout).toBe('');
  });
});

// A release workflow's step, cut out of it: its run block, dedented.
function stepRun(workflow: string, name: string): string {
  const lines = readFileSync(fileURLToPath(new URL(`../../../../../.github/workflows/${workflow}`, import.meta.url)), 'utf8').split('\n');
  const start = lines.indexOf(`      - name: ${name}`);
  expect(start, name).toBeGreaterThan(-1);
  const run = lines.findIndex((l, i) => i > start && /^\s+run: \|$/.test(l));
  const indent = /^\s*/.exec(lines[run + 1]!)![0];
  const body: string[] = [];
  for (const l of lines.slice(run + 1)) {
    if (l.trim() && !l.startsWith(indent)) break;
    body.push(l.slice(indent.length));
  }
  return body.join('\n');
}

// Whether this machine's openssl signs Ed25519 (OpenSSL 3's pkeyutl -rawin); macOS's LibreSSL and
// OpenSSL 1.1.1 don't, and CI's Ubuntu does.
const rawin = (() => {
  const dir = tempDir('akl-openssl-');
  try {
    execFileSync('openssl', ['genpkey', '-algorithm', 'ed25519', '-out', join(dir, 'k.pem')], { stdio: 'ignore' });
    writeFileSync(join(dir, 'm'), 'm');
    execFileSync('openssl', ['pkeyutl', '-sign', '-inkey', join(dir, 'k.pem'), '-rawin', '-in', join(dir, 'm'), '-out', join(dir, 's')], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
})();

// A generated Ed25519 key: its private PEM (the secret), public PEM (a keys/*.pub) and key id.
function releaseKey(): { secret: string; pub: string; id: string; key: KeyObject } {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  const der = publicKey.export({ type: 'spki', format: 'der' });
  return {
    secret: privateKey.export({ type: 'pkcs8', format: 'pem' }).toString(),
    pub: publicKey.export({ type: 'spki', format: 'pem' }).toString(),
    id: createHash('sha256').update(der.subarray(-32)).digest('hex').slice(0, 16),
    key: publicKey,
  };
}

// Each release workflow signs SHA256SUMS with the release environment's key, checks the signature
// against Meko's keys before anything is published, and uploads SHA256SUMS.sig with the rest.
describe.each(['agentk-release.yml', 'meko-release.yml', 'present-release.yml'])('%s’s signature', (name) => {
  const text = readFileSync(fileURLToPath(new URL(`../../../../../.github/workflows/${name}`, import.meta.url)), 'utf8');
  const publish = text.slice(text.indexOf('\n  publish:\n'));
  const steps = jobs(name).find((job) => job.some((s) => s[0] === '      - name: Sign SHA256SUMS'))!;
  const stepAt = (n: string) => steps.findIndex((s) => s[0] === `      - name: ${n}`);
  const sign = stepRun(name, 'Sign SHA256SUMS');
  const check = stepRun(name, "Check the signature against Meko's keys");
  const sums = 'abc  x.tar.gz\n';

  // Runs the sign step in dist-release/ with these keys, then the check step at the repo's root.
  const runSign = (keys: string[]) => {
    mkdirSync(join(repo, 'dist-release'), { recursive: true });
    writeFileSync(join(repo, 'dist-release', 'SHA256SUMS'), sums);
    return spawnSync('sh', ['-c', sign], {
      cwd: join(repo, 'dist-release'), encoding: 'utf8',
      env: { ...process.env, RELEASE_SIGNING_KEY: keys[0] ?? '', RELEASE_SIGNING_KEY_OLD: keys[1] ?? '' },
    });
  };
  const runCheck = () => spawnSync('sh', ['-c', check], {
    cwd: repo, encoding: 'utf8', env: { ...process.env, GITHUB_SHA: git('rev-parse', 'HEAD').trim() },
  });
  // Commits these public keys as Meko's, src/meko/internal/update/keys/<name>.pub.
  const commitKeys = (pubs: Record<string, string>) => {
    mkdirSync(join(repo, 'src/meko/internal/update/keys'), { recursive: true });
    for (const [n, pem] of Object.entries(pubs)) writeFileSync(join(repo, 'src/meko/internal/update/keys', `${n}.pub`), pem);
    git('add', 'src');
    commit('chore(meko): release keys');
  };
  const sigLines = () => readFileSync(join(repo, 'dist-release', 'SHA256SUMS.sig'), 'utf8').trimEnd().split('\n');

  it('signs in the publish job, with the release environment, before the release is created', () => {
    expect(publish).toMatch(/^ {4}environment: release$/m);
    expect(stepAt('Write SHA256SUMS')).toBeGreaterThan(-1);
    expect(stepAt('Sign SHA256SUMS')).toBe(stepAt('Write SHA256SUMS') + 1);
    expect(stepAt("Check the signature against Meko's keys")).toBe(stepAt('Sign SHA256SUMS') + 1);
    expect(stepAt('Create the release')).toBeGreaterThan(stepAt("Check the signature against Meko's keys"));
    expect(code(steps[stepAt('Sign SHA256SUMS')]!)).toContain('RELEASE_SIGNING_KEY: ${{ secrets.RELEASE_SIGNING_KEY }}');
  });

  it('uploads SHA256SUMS.sig with a new release and with a rebuild', () => {
    for (const n of ['Create the release', 'Replace assets on an existing release']) {
      expect(code(steps[stepAt(n)]!), n).toMatch(/dist-release\/SHA256SUMS dist-release\/SHA256SUMS\.sig/);
    }
  });

  it('refuses to sign with no key', () => {
    const r = runSign([]);
    expect(r.status).not.toBe(0);
    expect(r.stderr).toContain('RELEASE_SIGNING_KEY is empty');
  });

  it.skipIf(!rawin)('writes a line per key that Node verifies, and checks the first against Meko’s keys', () => {
    const a = releaseKey();
    const b = releaseKey();
    expect(runSign([a.secret]).status).toBe(0);
    const [line] = sigLines();
    const [id, b64] = line!.split(' ');
    expect(id).toBe(a.id);
    expect(verify(null, Buffer.from(sums), createPublicKey(a.pub), Buffer.from(b64!, 'base64'))).toBe(true);

    commitKeys({ a: a.pub, other: releaseKey().pub });
    expect(runCheck().status).toBe(0);

    // During a key change: two lines, the new key's first. A leaked old key's .pub is gone from
    // main while its line is still written, which the check allows.
    expect(runSign([b.secret, a.secret]).status).toBe(0);
    expect(sigLines().map((l) => l.split(' ')[0])).toEqual([b.id, a.id]);
    git('rm', '-q', 'src/meko/internal/update/keys/a.pub');
    commitKeys({ b: b.pub });
    expect(runCheck().status).toBe(0);
  });

  it.skipIf(!rawin)('fails the release when the secret isn’t one of Meko’s keys, or the signature was changed', () => {
    const a = releaseKey();
    commitKeys({ other: releaseKey().pub });
    expect(runSign([a.secret]).status).toBe(0);
    let r = runCheck();
    expect(r.status).toBe(1);
    expect(r.stderr).toContain(`RELEASE_SIGNING_KEY (key id ${a.id}) isn't one of Meko's keys`);

    commitKeys({ a: a.pub });
    const [id, b64] = sigLines()[0]!.split(' ');
    const sig = Buffer.from(b64!, 'base64');
    sig[0] ^= 1;
    writeFileSync(join(repo, 'dist-release', 'SHA256SUMS.sig'), `${id} ${sig.toString('base64')}\n`);
    r = runCheck();
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("SHA256SUMS.sig doesn't verify with src/meko/internal/update/keys/a.pub");
  });
});
