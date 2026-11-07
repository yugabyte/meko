import { execFileSync } from 'node:child_process';
import { mkdirSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { PROMPT_MAX } from '../../src/server/actions.js';
import { buildDemoRepo, demoMekoMd, freeFolder, levelPrompts, listBundles, readWorkshop, renderGuide } from '../../src/server/workshops.js';
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

  it("renders the host's script with every placeholder filled and each level's prompt in it", () => {
    for (const b of bundles) {
      const text = renderGuide(b, { projectId: 'p1', repo: '/code/demo', meko: 'on', harness: 'Claude Code', gotchas: 'adlc/repo/demo/gotchas', datapack: 'adlc/proj/demo/init' });
      expect(text).not.toMatch(/\{\{\w+\}\}/);
      for (const l of b.levels) {
        for (const p of levelPrompts(b, l.id)) {
          expect(text).toContain(p.text);
          // A named session's prompt goes under its name, which the host starts it with.
          if (p.name) expect(text).toContain(`#### Session "${p.name}"`);
        }
      }
    }
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
    const write = (levels: unknown[]) => {
      mkdirSync(join(root, 'demo'), { recursive: true });
      writeFileSync(join(root, 'demo', 'workshop.json'), JSON.stringify({
        id: 'demo', title: 'Demo', summary: 's', guide: 'GUIDE.md',
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
  });

  it('builds the demo: main with the bug and MEKO.md, the teammate\'s fix on its branch, and no gotchas in the repo', async () => {
    const b = readWorkshop(AGENTK_TOUR);
    const parent = tmp();
    const dest = freeFolder(parent, b.repo.folder);
    const dps = [{ name: 'adlc/repo/kode-demo/gotchas', id: '0f6b1c1e-0000-4000-8000-00000000a001', about: 'gotchas' }];
    await buildDemoRepo(b, dest, demoMekoMd(dps));
    // A second one goes beside it.
    mkdirSync(join(parent, `${b.repo.folder}-2`));
    expect(freeFolder(parent, b.repo.folder)).toBe(join(parent, `${b.repo.folder}-3`));

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
