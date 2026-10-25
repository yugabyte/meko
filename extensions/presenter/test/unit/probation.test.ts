import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { markHealthy, pendingFile, uncountStart } from '../../src/server/probation.js';
import { tempDir } from '../helpers.js';

let app: string;
beforeEach(() => {
  app = join(tempDir('pres-prob-'), 'app');
  mkdirSync(join(app, '0.20261025.0'), { recursive: true });
});
afterEach(() => rmSync(join(app, '..'), { recursive: true, force: true }));

const appDir = () => join(app, '0.20261025.0');
// As Meko's update writes it, with its own fields kept.
const record = (to: string, attempts: number) =>
  writeFileSync(join(app, 'update-pending.json'), JSON.stringify({ from: '0.20261024.0', to, schema: 0, attempts, at: '2026-10-24T00:00:00Z' }));
const read = () => JSON.parse(readFileSync(join(app, 'update-pending.json'), 'utf8')) as Record<string, unknown>;

describe('a release on probation', () => {
  it('keeps its record beside the release folders', () => {
    expect(pendingFile(appDir())).toBe(join(app, 'update-pending.json'));
  });

  it.each([
    ['the running version', '0.20261025.0', false],
    ['another version', '0.20261026.0', true],
  ])('a healthy server clears the record of %s only', (_, to, kept) => {
    record(to, 1);
    markHealthy(appDir());
    expect(existsSync(join(app, 'update-pending.json'))).toBe(kept);
  });

  it.each([
    ['takes a clean stop off the count', '0.20261025.0', 2, 1],
    ['never counts below zero', '0.20261025.0', 0, 0],
    ['leaves another version\'s count alone', '0.20261026.0', 2, 2],
  ])('%s', (_, to, attempts, want) => {
    record(to, attempts);
    uncountStart(appDir());
    expect(read()).toEqual({ from: '0.20261024.0', to, schema: 0, attempts: want, at: '2026-10-24T00:00:00Z' });
  });

  it('does nothing for a build from source (no release folder) or with no record', () => {
    expect(() => {
      markHealthy(undefined);
      uncountStart(undefined);
      markHealthy(appDir());
      uncountStart(appDir());
    }).not.toThrow();
  });
});
