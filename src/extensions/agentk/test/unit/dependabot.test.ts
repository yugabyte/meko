import { spawnSync, execFileSync } from 'node:child_process';
import { readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { gitRepo, tempDir } from '../helpers.js';

// Dependabot's commit subjects become its PR titles and decide whether merging its PRs cuts a
// release, so check the prefixes in the repo root's .github/dependabot.yml against the real
// scripts/check-title.sh (at the repo root) and scripts/release/release-notes.sh.
const CONFIG = readFileSync(fileURLToPath(new URL('../../../../../.github/dependabot.yml', import.meta.url)), 'utf8');
const NOTES = fileURLToPath(new URL('../../../../../scripts/release/release-notes.sh', import.meta.url));
const CHECK = fileURLToPath(new URL('../../../../../scripts/check-title.sh', import.meta.url));

interface Group {
  name: string;
  patterns: string[];
}

interface Update {
  ecosystem: string;
  directory: string;
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
      out.push({ ecosystem: eco[1]!, directory: '', scope: false, assignees: [], groups: [] });
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
    u.directory = /^ {4}directory: (\S+)/.exec(line)?.[1] ?? u.directory;
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

const byDirectory = (dir: string) => {
  const u = updates().find((x) => x.directory === dir);
  if (!u) throw new Error(`no entry for ${dir} in .github/dependabot.yml`);
  return u;
};
const AGENTK = '/src/extensions/agentk';
const WEB_UI = '/src/meko/web_ui';

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
const notes = (product = 'agentk') => spawnSync('sh', [NOTES, '--product', product], { cwd: repo, encoding: 'utf8' });
// Dependabot's subjects run past the 72-character limit, so it's exempt from that one rule.
const checkTitle = (title: string) => spawnSync('sh', [CHECK, '--no-length', title], { encoding: 'utf8' });

describe('dependabot config', () => {
  it("covers AgentK's npm, Meko's Go modules, context engine and web UI, and GitHub Actions, assigns every PR, and sets its commit prefixes", () => {
    expect(updates().map((u) => `${u.ecosystem} ${u.directory}`).sort()).toEqual(
      ['github-actions /', 'gomod /src/meko', `npm ${AGENTK}`, 'npm /src/meko/context_engine', `npm ${WEB_UI}`]);
    for (const u of updates()) {
      expect(u.assignees, u.directory).not.toEqual([]);
      // Without an explicit prefix Dependabot guesses one from the repo's history.
      expect(u.prefix, u.directory).toBeTruthy();
      if (u.ecosystem === 'npm') expect(u.prefixDev, u.directory).toBeTruthy();
    }
  });

  it('writes subjects that pass the title rules', () => {
    for (const u of updates()) {
      // A scope would add (deps) after the prefix's component, e.g. deps(agentk)(deps): ...
      expect(u.scope, u.directory).toBe(false);
      for (const dev of u.ecosystem === 'npm' ? [false, true] : [false]) {
        const s = subject(u, dev, 'bump the production group across 1 directory with 12 updates');
        const r = checkTitle(s);
        expect(r.status, `${s}\n${r.stderr}`).toBe(0);
      }
    }
  });

  // Only AgentK's manifest is read here: a root file these tests read runs AgentK's CI job
  // (test/unit/ci-scope.test.ts), and the web UI's bumps shouldn't.
  it.each([
    [AGENTK, [
      ['fastify', '@fastify/static', '@fastify/websocket'],
      ['react', 'react-dom', '@types/react', '@types/react-dom'],
      ['@xterm/xterm', '@xterm/addon-fit', '@xterm/addon-web-links', '@xterm/addon-webgl'],
      ['vite', '@vitejs/plugin-react', 'vitest'],
      ['typescript', 'typescript-eslint'],
      ['eslint', '@eslint/js', 'eslint-plugin-react-hooks'],
    ]],
    [WEB_UI, [
      ['react', 'react-dom', '@types/react', '@types/react-dom'],
      ['vite', '@vitejs/plugin-react', 'vitest', '@tailwindcss/vite', 'tailwindcss'],
    ]],
  ])('keeps packages with peer ranges on each other in one group in %s', (dir, families) => {
    const npm = byDirectory(dir);
    let deps: Set<string> | undefined;
    if (dir === AGENTK) {
      const pkg = JSON.parse(readFileSync(fileURLToPath(new URL('../../package.json', import.meta.url)), 'utf8')) as {
        dependencies: Record<string, string>;
        devDependencies: Record<string, string>;
      };
      deps = new Set([...Object.keys(pkg.dependencies), ...Object.keys(pkg.devDependencies)]);
    }
    for (const family of families) {
      if (deps) for (const p of family) expect(deps, `${p} is no longer a dependency`).toContain(p);
      const groups = new Set(family.map((p) => groupOf(npm, p)));
      expect([...groups], family.join(', ')).toHaveLength(1);
      expect(groups.has(undefined), family.join(', ')).toBe(false);
    }
  });

  it('releases runtime and web UI dependency bumps', () => {
    const npm = byDirectory(AGENTK);
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

  // Nothing in src/meko ships in AgentK's release, so Meko's bumps don't release AgentK.
  it.each([
    ['/', false, 'bump actions/checkout from 4 to 5'],
    ['/src/meko', false, 'bump github.com/labstack/echo/v4 from 4.16.0 to 4.17.0'],
    ['/src/meko/context_engine', false, 'bump mem0ai from 3.1.2 to 3.1.3'],
    ['/src/meko/context_engine', true, 'bump tsx from 4.23.15 to 4.23.16'],
    [WEB_UI, false, 'bump lucide-react from 0.545.0 to 0.546.0'],
    [WEB_UI, true, 'bump happy-dom from 21.0.2 to 21.0.4'],
  ])('does not release AgentK for a bump in %s (dev %s) on its own', (dir, dev, what) => {
    commit(subject(byDirectory(dir), dev, what));
    expect(notes().status).toBe(3);
  });

  // Meko bundles its Go modules, the context engine's production packages and the web UI's
  // dependencies (built into web_ui/dist); dev-only tools ship with its next release.
  it.each([
    ['/src/meko', false, 'bump github.com/labstack/echo/v4 from 4.16.0 to 4.17.0', true],
    ['/src/meko/context_engine', false, 'bump mem0ai from 3.1.2 to 3.1.3', true],
    ['/src/meko/context_engine', true, 'bump tsx from 4.23.15 to 4.23.16', false],
    [WEB_UI, false, 'bump lucide-react from 0.545.0 to 0.546.0', true],
    [WEB_UI, true, 'bump happy-dom from 21.0.2 to 21.0.4', false],
    ['/', false, 'bump actions/checkout from 4 to 5', false],
  ])('decides whether a bump in %s (dev %s) releases Meko', (dir, dev, what, releases) => {
    execFileSync('git', ['tag', 'meko-v0.20260101.0'], { cwd: repo });
    commit(subject(byDirectory(dir), dev, what));
    const r = notes('meko');
    expect(r.status, r.stderr).toBe(releases ? 0 : 3);
    if (releases) expect(r.stdout).toContain(what);
  });
});
