import { execFileSync } from 'node:child_process';
import { mkdirSync, readdirSync, readFileSync, statSync, symlinkSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { PROMPT_MAX } from '../../src/server/actions.js';
import { buildDemoRepo, demoMekoMd, freeFolder, gotchaList, levelPrompts, listBundles, readWorkshop } from '../../src/server/workshops.js';
import { fillText, inlineAssets, insideRoot, withBar } from '../../src/server/tour.js';
import { scoreDifference, sideText, tokensText } from '../../src/shared/tour.js';
import { parseMekoMd } from '../../src/shared/mekomd.js';
import { AGENTK_TOUR } from '../../src/shared/types.js';

const git = (dir: string, ...args: string[]) => execFileSync('git', args, { cwd: dir, encoding: 'utf8' }).trim();
const tmp = () => mkdtempSync(join(tmpdir(), 'akl-workshop-'));

/** Every file under <dir>, relative to it. */
function files(dir: string, base = dir): string[] {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n);
    return statSync(p).isDirectory() ? files(p, base) : [relative(base, p)];
  });
}

interface DemoState { score: number; best: number; snake: { x: number; y: number }[]; food: { x: number; y: number } }
interface DemoGame {
  create(cols: number, rows: number, o?: { rand?: () => number }): DemoState;
  newGame(s: DemoState): void;
  step(s: DemoState): void;
}

describe('the bundled workshops', () => {
  const bundles = listBundles();

  it('includes the Kode tour, and each one reads, with its files where workshop.json says', () => {
    expect(bundles.map((b) => b.id)).toContain(AGENTK_TOUR);
    for (const b of bundles) {
      expect(readWorkshop(b.id)).toEqual(b);
      for (const l of b.levels) {
        for (const p of levelPrompts(b, l.id)) expect(p.text.length, `${b.id} ${l.id} ${p.name ?? ''}`).toBeLessThanOrEqual(PROMPT_MAX);
      }
      for (const br of b.repo.branches) expect(statSync(join(b.root, br.patch)).isFile()).toBe(true);
      if (b.gotchas) expect(statSync(join(b.root, b.gotchas.file)).isFile()).toBe(true);
      // The game opens from disk (file://), where browsers refuse module scripts and fetch; no dot-file
      // in it could apply to Kode's own repo.
      for (const f of files(join(b.root, b.repo.dir))) {
        expect(f, f).not.toMatch(/(^|\/)\./);
        if (/\.(js|html)$/.test(f)) expect(readFileSync(join(b.root, b.repo.dir, f), 'utf8'), f).not.toMatch(/type="module"|^\s*(import|export)\s|\bfetch\(/m);
      }
    }
  });

  it("lists a gotchas file's ids and titles from its headings, and none without one", () => {
    const root = tmp();
    writeFileSync(join(root, 'gotchas.md'), '# Gotchas\n\n## MG-1: The first trap\n\nWhy.\n\n## MG-2 The second trap\n\n### Not a gotcha\n');
    const b = { ...readWorkshop(AGENTK_TOUR), root, gotchas: { file: 'gotchas.md', datapack: 'adlc/repo/demo/gotchas', about: '' } };
    expect(gotchaList(b)).toEqual([{ id: 'MG-1', title: 'The first trap' }, { id: 'MG-2', title: 'The second trap' }]);
    expect(gotchaList({ ...b, gotchas: null })).toEqual([]);
  });

  it("keeps Level 2's answer fixed: the demo's tests cover neither running into itself nor a full board", () => {
    const b = readWorkshop(AGENTK_TOUR);
    const tests = readFileSync(join(b.root, b.repo.dir, 'test', 'game.test.js'), 'utf8');
    const names = [...tests.matchAll(/^test\('([^']+)'/gm)].map((m) => m[1]);
    expect(names.length).toBeGreaterThan(0);
    for (const n of names) expect(n).not.toMatch(/(runs?|running|hits?|hitting) into itself|hitting itself|own body|full board|no food|board is full/i);
    // The rules are there: the tests just don't cover them.
    const game = readFileSync(join(b.root, b.repo.dir, 'src', 'game.js'), 'utf8');
    expect(game).toMatch(/none when the board is full/);
    expect(game).toMatch(/the game ends on a wall or itself/);
  });

  it('reads a level prompt list and a learn brief, and refuses bad ones', () => {
    const root = tmp();
    const REPO_STAGE = [{ id: 'repo', kind: 'repo', title: 'The repo', text: { done: 'A game.' } }];
    const write = (levels: Record<string, unknown>[]) => {
      levels = levels.map((l) => ({ stages: REPO_STAGE, ...l }));
      mkdirSync(join(root, 'demo'), { recursive: true });
      writeFileSync(join(root, 'demo', 'workshop.json'), JSON.stringify({
        id: 'demo', title: 'Demo', summary: 's', end: 'The end.',
        repo: { dir: 'repo', folder: 'demo', author: { name: 'A', email: 'a@example.com' }, date: '2026-01-01T00:00:00Z', message: 'm', branches: [] },
        levels,
      }));
      return () => readWorkshop('demo', root);
    };
    expect(write([{ id: 'a', title: 'A', prompt: 'levels/a.md' }])().levels[0]).toMatchObject({ prompts: [{ name: null, file: 'levels/a.md', learn: false }], learn: null });
    expect(write([{ id: 'a', title: 'A', prompt: [{ name: 'One', file: 'one.md', learn: true }, { name: 'Two', file: 'two.md' }], learn: { about: 'A game', where: 'src', months: 0 } }])().levels[0])
      .toMatchObject({ prompts: [{ name: 'One', learn: true }, { name: 'Two', learn: false }], learn: { about: 'A game', where: 'src', months: 0 } });
    expect(write([{ id: 'a', title: 'A', prompt: [] }])).toThrow(/prompt is missing/);
    expect(write([{ id: 'a', title: 'A', prompt: [{ name: 'One', file: 'one.md' }, { name: 'one', file: 'two.md' }] }])).toThrow(/share a name/);
    expect(write([{ id: 'a', title: 'A', prompt: 'a.md', learn: { about: '', months: 0 } }])).toThrow(/learn brief/);
    expect(write([{ id: 'a', title: 'A', prompt: 'a.md', learn: { about: 'x', months: 999 } }])).toThrow(/learn brief/);
    // Stages: a known kind, the text it needs, known placeholders, and what their kind needs of the level.
    const stage = (st: Record<string, unknown>) => write([{ id: 'a', title: 'A', prompt: 'a.md', stages: [st] }]);
    expect(write([{ id: 'a', title: 'A', prompt: 'a.md', stages: [] }])).toThrow(/no stages/);
    expect(stage({ id: 's', kind: 'dance', title: 'S', text: { done: 'd' } })).toThrow(/unknown kind/);
    expect(stage({ id: 's', kind: 'teach', title: 'S', text: { done: 'd' } })).toThrow(/ready and running/);
    expect(stage({ id: 's', kind: 'repo', title: 'S', text: { done: 'In {{nowhere}}.' } })).toThrow(/unknown placeholder \{\{nowhere\}\}/);
    expect(stage({ id: 's', kind: 'review-context', title: 'S', text: { ready: 'r', running: 'r', done: 'd' } })).toThrow(/needs its level's review/);
    expect(stage({ id: 's', kind: 'learn', title: 'S', text: { ready: 'r', running: 'r', done: 'd' } })).toThrow(/learn brief/);
    expect(write([{ id: 'a', title: 'A', prompt: 'a.md', stages: [...REPO_STAGE, ...REPO_STAGE] }])).toThrow(/share an id/);
    expect(stage({ id: 's', kind: 'repo', title: 'S', text: { done: 'In {{repo}}.' }, labels: { next: 'Go', other: 'x' } })().levels[0].stages[0])
      .toEqual({ id: 's', kind: 'repo', title: 'S', text: { done: 'In {{repo}}.' }, labels: { next: 'Go' } });
  });

  it('builds the demo: main with the bug and MEKO.md, the teammate\'s fix on its branch, and no gotchas in the repo', async () => {
    const b = readWorkshop(AGENTK_TOUR);
    const parent = tmp();
    const dest = freeFolder(parent, b.repo.folder);
    const dps = [{ name: 'adlc/repo/kode-demo/gotchas', id: '0f6b1c1e-0000-4000-8000-00000000a001', about: 'gotchas' }];
    await buildDemoRepo(b, dest, demoMekoMd(dps));
    // A second one goes beside it, past a link to nowhere too.
    mkdirSync(join(parent, `${b.repo.folder}-2`));
    symlinkSync(join(parent, 'nowhere'), join(parent, `${b.repo.folder}-3`));
    expect(freeFolder(parent, b.repo.folder)).toBe(join(parent, `${b.repo.folder}-4`));

    expect(git(dest, 'branch', '--format=%(refname:short)').split('\n').sort()).toEqual(['demo/score-fix', 'main']);
    expect(git(dest, 'status', '--porcelain')).toBe('');
    for (const branch of ['main', 'demo/score-fix']) {
      expect(parseMekoMd(git(dest, 'show', `${branch}:MEKO.md`))).toEqual(dps);
      expect(git(dest, 'ls-tree', '-r', '--name-only', branch)).not.toMatch(/gotchas/);
    }
    // The bug on main, and the teammate's fix (which falls into MG-2) on the branch.
    const game = (branch: string): DemoGame => {
      const dir = join(tmp(), branch.replace('/', '-'));
      git(dest, 'worktree', 'add', '-q', '--detach', dir, branch);
      return createRequire(import.meta.url)(join(dir, 'src', 'game.js')) as DemoGame;
    };
    const scoreAfterNewGame = (g: DemoGame) => {
      const s = g.create(10, 10, { rand: () => 0 });
      s.food = { x: s.snake[0].x + 1, y: s.snake[0].y };
      g.step(s);
      expect(s.score).toBe(1);
      g.newGame(s);
      return s.score;
    };
    expect(scoreAfterNewGame(game('main'))).toBe(1);
    expect(scoreAfterNewGame(game('demo/score-fix'))).toBe(0);
  });
});

describe("the tour's pure parts", () => {
  it('says tokens and credits short', () => {
    expect(tokensText({ tokensIn: 41234, credits: 0 })).toBe('41.2k tokens in');
    expect(tokensText({ tokensIn: 999, credits: 0 })).toBe('999 tokens in');
    expect(tokensText({ tokensIn: 2_500_000, credits: 0 })).toBe('2.5M tokens in');
    expect(tokensText({ tokensIn: 0, credits: 3.456 })).toBe('3.46 credits');
    expect(sideText(null)).toBe('—');
    expect(sideText({ kept: 1, caught: 1, gotchas: ['MG-2'], tokensIn: 1000, credits: 0 })).toBe('1 issue');
  });

  it("gives the scoreboard's difference: what Meko caught", () => {
    const side = (caught: number) => ({ kept: 2, caught, gotchas: caught ? ['MG-2'] : [], tokensIn: 40000, credits: 0 });
    expect(scoreDifference(side(1))).toBe('Caught with Meko: MG-2');
    expect(scoreDifference(side(0))).toBe('Nothing caught with Meko this time');
  });

  it('fills a stage\'s text, leaving a placeholder with no value as it is', () => {
    expect(fillText(' Found {{cold.kept}} in {{repo}}. {{failure}} ', { 'cold.kept': '2 issues', repo: '~/code/kode-demo' })).toBe('Found 2 issues in ~/code/kode-demo. {{failure}}');
  });

  it('plays only real files inside the demo, and puts its scripts and stylesheets inline', () => {
    const root = join(tmp(), 'demo');
    mkdirSync(join(root, 'src'), { recursive: true });
    mkdirSync(join(root, '.git'));
    writeFileSync(join(root, 'index.html'), '<link rel="stylesheet" href="style.css">\n<script src="src/game.js"></script>\n<script src="https://cdn.example.com/x.js"></script>\n<script src="../outside.js"></script>');
    writeFileSync(join(root, 'style.css'), 'body { color: red }');
    writeFileSync(join(root, 'src', 'game.js'), 'var Game = 1; // </script> stays inside');
    writeFileSync(join(root, '.git', 'config'), 'secret');
    writeFileSync(join(root, '..', 'outside.js'), 'outside');
    expect(insideRoot(root, 'index.html')).toBeTruthy();
    for (const bad of ['../outside.js', '.git/config', 'src/../../outside.js', 'missing.js', 'src', '']) expect(insideRoot(root, bad), bad).toBeNull();
    const html = inlineAssets(readFileSync(join(root, 'index.html'), 'utf8'), root, root);
    expect(html).toContain('<style>\nbody { color: red }\n</style>');
    expect(html).toContain('<script>\nvar Game = 1; // <\\/script> stays inside\n</script>');
    // Another site's script, and one outside the demo, are left as they are.
    expect(html).toContain('<script src="https://cdn.example.com/x.js"></script>');
    expect(html).toContain('<script src="../outside.js"></script>');
    // The workshop's line goes in a bar right after <body>, escaped.
    expect(withBar('<html><body class="x"><main></main></body></html>', 'Go back to <Kode> & play')).toMatch(/^<html><body class="x"><div role="note"[^>]*>Go back to &lt;Kode&gt; &amp; play<\/div><main>/);
    expect(withBar('<main></main>', null)).toBe('<main></main>');
  });
});
