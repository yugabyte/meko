// packaging/meko_presenter, the release's launcher, in a release install laid out as Meko lays it
// out: app/<version>/{bin/meko_presenter, node/bin/node, lib/dist/server/cli.js}, app/current, and the update
// record Meko writes as it switches versions. Each version's cli.js is a stub that says which
// version ran, with what.
import { spawnSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, readFileSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';

const LAUNCHER = fileURLToPath(new URL('../../packaging/meko_presenter', import.meta.url));
const OLD = '0.20261024.0';
const NEW = '0.20261025.0';
let dir: string;
let app: string;

function release(version: string): void {
  const v = join(app, version);
  mkdirSync(join(v, 'bin'), { recursive: true });
  mkdirSync(join(v, 'node', 'bin'), { recursive: true });
  mkdirSync(join(v, 'lib', 'dist', 'server'), { recursive: true });
  copyFileSync(LAUNCHER, join(v, 'bin', 'meko_presenter'));
  symlinkSync(process.execPath, join(v, 'node', 'bin', 'node'));
  writeFileSync(join(v, 'lib', 'dist', 'server', 'cli.js'),
    `console.log(JSON.stringify({ version: ${JSON.stringify(version)}, args: process.argv.slice(2), appDir: process.env.PRESENTER_APP_DIR, launcher: process.env.PRESENTER_LAUNCHER }));\n`);
}

beforeEach(() => {
  dir = tempDir('pres-launch-');
  app = join(dir, 'app');
  release(OLD);
  release(NEW);
  symlinkSync(NEW, join(app, 'current'));
  // ~/.local/bin/meko_presenter, as Meko links it.
  mkdirSync(join(dir, 'bin'));
  symlinkSync(join(app, 'current', 'bin', 'meko_presenter'), join(dir, 'bin', 'meko_presenter'));
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

function presenter(...args: string[]) {
  const r = spawnSync('sh', [join(dir, 'bin', 'meko_presenter'), ...args], { encoding: 'utf8' });
  expect(r.status, r.stderr).toBe(0);
  return { ran: JSON.parse(r.stdout) as { version: string; args: string[]; appDir: string; launcher: string }, stderr: r.stderr };
}
const record = (attempts: number) =>
  writeFileSync(join(app, 'update-pending.json'), JSON.stringify({ from: OLD, to: NEW, schema: 0, attempts }));
const pending = () => existsSync(join(app, 'update-pending.json'))
  ? (JSON.parse(readFileSync(join(app, 'update-pending.json'), 'utf8')) as { attempts: number })
  : null;

describe('the release launcher', () => {
  it('runs the bundled server of the release app/current names, naming its folder and itself', () => {
    const { ran } = presenter('status');
    expect(ran).toEqual({ version: NEW, args: ['status'], appDir: join(app, NEW), launcher: join(app, NEW, 'bin', 'meko_presenter') });
  });

  it('counts the starts of the version on probation, and only serve\'s', () => {
    record(0);
    presenter('status');
    expect(pending()?.attempts).toBe(0);
    presenter('serve');
    presenter('serve');
    expect(pending()?.attempts).toBe(2);
  });

  it('rolls back after three failed starts: the old version is current and runs, the new one is bad', () => {
    record(3);
    const { ran, stderr } = presenter('serve');
    expect(ran.version).toBe(OLD);
    expect(ran.args).toEqual(['serve']);
    expect(stderr).toContain(`version ${NEW} failed to start 3 times; rolling back to ${OLD}`);
    expect(readlinkSync(join(app, 'current'))).toBe(OLD);
    expect(readFileSync(join(app, 'bad-versions'), 'utf8')).toBe(`${NEW}\n`);
    expect(pending()).toBeNull();
  });

  it('drops the record when the old version starts (switched back by hand)', () => {
    record(1);
    rmSync(join(app, 'current'));
    symlinkSync(OLD, join(app, 'current'));
    expect(presenter('serve').ran.version).toBe(OLD);
    expect(pending()).toBeNull();
  });

  it('keeps counting when the old version is gone: there is nothing to roll back to', () => {
    record(3);
    rmSync(join(app, OLD), { recursive: true });
    expect(presenter('serve').ran.version).toBe(NEW);
    expect(pending()?.attempts).toBe(4);
  });
});
