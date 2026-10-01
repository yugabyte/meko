import { spawnSync, execFileSync } from 'node:child_process';
import { readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';

// Dependabot's commit subjects become its PR titles and decide whether merging its PRs cuts a
// release, so check the prefixes in .github/dependabot.yml against the real
// scripts/check-title.sh and scripts/release-notes.sh.
const CONFIG = readFileSync(fileURLToPath(new URL('../../.github/dependabot.yml', import.meta.url)), 'utf8');
const NOTES = fileURLToPath(new URL('../../scripts/release-notes.sh', import.meta.url));
const CHECK = fileURLToPath(new URL('../../scripts/check-title.sh', import.meta.url));

interface Group {
  name: string;
  patterns: string[];
}

interface Update {
  ecosystem: string;
  prefix?: string;
  prefixDev?: string;
  scope: boolean;
  assignees: string[];
  groups: Group[];
}

/** The `updates` entries, read line by line (the config is flat enough not to need a YAML parser). */
function updates(): Update[] {
  const out: Update[] = [];
  let list: string[] | null = null;
  let section = '';
  for (const line of CONFIG.split('\n')) {
    const eco = /^ {2}- package-ecosystem: (\S+)/.exec(line);
    if (eco) {
      out.push({ ecosystem: eco[1]!, scope: false, assignees: [], groups: [] });
      list = null;
      continue;
    }
    const u = out.at(-1);
    if (!u || /^\s*(#|$)/.test(line)) continue;
    const item = /^ {6}- (\S+)$/.exec(line);
    if (list && item) {
      list.push(item[1]!);
      continue;
    }
    list = /^ {4}assignees:$/.test(line) ? u.assignees : null;
    section = /^ {4}(\S+):/.exec(line)?.[1] ?? section;
    const kv = /^ {6}(prefix|prefix-development|include): (\S+)/.exec(line);
    if (kv?.[1] === 'prefix') u.prefix = kv[2];
    if (kv?.[1] === 'prefix-development') u.prefixDev = kv[2];
    if (kv?.[1] === 'include') u.scope = kv[2] === 'scope';
    const group = /^ {6}([\w-]+):$/.exec(line);
    if (section === 'groups' && group) u.groups.push({ name: group[1]!, patterns: [] });
    const patterns = /^ {8}patterns: \[(.*)\]$/.exec(line);
    if (section === 'groups' && patterns) u.groups.at(-1)!.patterns = [...patterns[1]!.matchAll(/"([^"]+)"/g)].map((m) => m[1]!);
  }
  return out;
}

/** The pattern group a package lands in (Dependabot takes the first one that matches). */
function groupOf(u: Update, pkg: string): string | undefined {
  const glob = (p: string) => new RegExp(`^${p.replace(/[.+?^${}()|[\]\\/]/g, '\\$&').replace(/\*/g, '.*')}$`);
  return u.groups.find((g) => g.patterns.some((p) => glob(p).test(pkg)))?.name;
}

/** A commit subject as Dependabot writes it for this update entry. */
function subject(u: Update, dev: boolean, what: string): string {
  const type = dev && u.ecosystem === 'npm' ? u.prefixDev : u.prefix;
  return `${type}${u.scope ? (dev ? '(deps-dev)' : '(deps)') : ''}: ${what}`;
}

const byEcosystem = (name: string) => {
  const u = updates().find((x) => x.ecosystem === name);
  if (!u) throw new Error(`no ${name} entry in .github/dependabot.yml`);
  return u;
};

let root: string;
let repo: string;
beforeEach(() => {
  root = tempDir('akl-dependabot-');
  repo = gitRepo(join(root, 'repo'));
  execFileSync('git', ['tag', 'v0.20260101.0'], { cwd: repo });
});
afterEach(() => rmSync(root, { recursive: true, force: true }));

function commit(msg: string) {
  execFileSync('git', ['commit', '-q', '--allow-empty', '-m', msg], { cwd: repo });
}
const notes = () => spawnSync('sh', [NOTES], { cwd: repo, encoding: 'utf8' });
// Dependabot's subjects run past the 72-character limit, so it's exempt from that one rule.
const checkTitle = (title: string) => spawnSync('sh', [CHECK, '--no-length', title], { encoding: 'utf8' });

describe('dependabot config', () => {
  it('covers npm and GitHub Actions, assigns every PR, and sets its commit prefixes', () => {
    expect(updates().map((u) => u.ecosystem).sort()).toEqual(['github-actions', 'npm']);
    for (const u of updates()) {
      expect(u.assignees, u.ecosystem).not.toEqual([]);
      // Without an explicit prefix Dependabot guesses one from the repo's history.
      expect(u.prefix, u.ecosystem).toBeTruthy();
    }
    expect(byEcosystem('npm').prefixDev).toBeTruthy();
  });

  it('writes subjects that pass the title rules', () => {
    for (const u of updates()) {
      // A scope would add (deps) to the type, e.g. deps(deps): ...
      expect(u.scope, u.ecosystem).toBe(false);
      for (const dev of u.ecosystem === 'npm' ? [false, true] : [false]) {
        const s = subject(u, dev, 'bump the production group across 1 directory with 12 updates');
        const r = checkTitle(s);
        expect(r.status, `${s}\n${r.stderr}`).toBe(0);
      }
    }
  });

  it('keeps packages with peer ranges on each other in one group', () => {
    const npm = byEcosystem('npm');
    const pkg = JSON.parse(readFileSync(fileURLToPath(new URL('../../package.json', import.meta.url)), 'utf8')) as {
      dependencies: Record<string, string>;
      devDependencies: Record<string, string>;
    };
    const deps = new Set([...Object.keys(pkg.dependencies), ...Object.keys(pkg.devDependencies)]);
    const families = [
      ['fastify', '@fastify/cookie', '@fastify/static', '@fastify/websocket'],
      ['react', 'react-dom', '@types/react', '@types/react-dom'],
      ['@xterm/xterm', '@xterm/addon-fit', '@xterm/addon-web-links', '@xterm/addon-webgl'],
      ['vite', '@vitejs/plugin-react', 'vitest'],
      ['typescript', 'typescript-eslint'],
      ['eslint', '@eslint/js', 'eslint-plugin-react-hooks'],
    ];
    for (const family of families) {
      for (const p of family) expect(deps, `${p} is no longer a dependency`).toContain(p);
      const groups = new Set(family.map((p) => groupOf(npm, p)));
      expect([...groups], family.join(', ')).toHaveLength(1);
      expect(groups.has(undefined), family.join(', ')).toBe(false);
    }
  });

  it('releases runtime and web UI dependency bumps', () => {
    const npm = byEcosystem('npm');
    commit(subject(npm, false, 'bump fastify from 5.12.5 to 5.13.0'));
    let r = notes();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('### Dependencies');
    expect(r.stdout).toContain('bump fastify from 5.12.5 to 5.13.0');

    execFileSync('git', ['tag', 'v0.20260101.1'], { cwd: repo });
    commit(subject(npm, true, 'bump dompurify from 3.4.16 to 3.4.17'));
    r = notes();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('bump dompurify from 3.4.16 to 3.4.17');
  });

  it('does not release action bumps on their own', () => {
    commit(subject(byEcosystem('github-actions'), false, 'bump actions/checkout from 4 to 5'));
    expect(notes().status).toBe(3);
  });
});
