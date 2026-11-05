// packaging/me_presenter, the release's launcher, in a release install laid out as Meko lays it
// out: app/<version>/{bin/me_presenter, bin/meko_presenter (its name before, a link), node/bin/me_presenter,
// lib/dist/server/cli.js} and app/current.
// Each version's cli.js is a stub that says which version ran, with what. A fake `meko` on PATH
// stands in for Meko's launcher, which counts the starts of an update and rolls one back.
import { spawnSync } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';

const LAUNCHER = fileURLToPath(new URL('../../packaging/me_presenter', import.meta.url));
const NEW = '0.20261025.0';
let dir: string;
let app: string;

beforeEach(() => {
  dir = tempDir('pres-launch-');
  app = join(dir, 'app');
  const v = join(app, NEW);
  mkdirSync(join(v, 'bin'), { recursive: true });
  mkdirSync(join(v, 'node', 'bin'), { recursive: true });
  mkdirSync(join(v, 'lib', 'dist', 'server'), { recursive: true });
  copyFileSync(LAUNCHER, join(v, 'bin', 'me_presenter'));
  symlinkSync('me_presenter', join(v, 'bin', 'meko_presenter'));
  symlinkSync(process.execPath, join(v, 'node', 'bin', 'me_presenter'));
  writeFileSync(join(v, 'lib', 'dist', 'server', 'cli.js'),
    `console.log(JSON.stringify({ version: ${JSON.stringify(NEW)}, args: process.argv.slice(2), appDir: process.env.PRESENTER_APP_DIR, launcher: process.env.PRESENTER_LAUNCHER, launched: process.env.MEKO_LAUNCHED ?? null }));\n`);
  symlinkSync(NEW, join(app, 'current'));
  // ~/.local/bin/me_presenter, as Meko links it, and meko_presenter, as an older Meko did.
  mkdirSync(join(dir, 'bin'));
  symlinkSync(join(app, 'current', 'bin', 'me_presenter'), join(dir, 'bin', 'me_presenter'));
  symlinkSync(join(app, 'current', 'bin', 'meko_presenter'), join(dir, 'bin', 'meko_presenter'));
  mkdirSync(join(dir, 'home'));
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

/**
 * A fake meko in its own folder on PATH: `_launch --supported` answers as `supported` says, and
 * `_launch` records its arguments, then runs the launcher it names again, as Meko does.
 */
function fakeMeko(supported: boolean): void {
  mkdirSync(join(dir, 'meko-bin'));
  const meko = join(dir, 'meko-bin', 'meko');
  writeFileSync(meko, `#!/bin/sh
[ "$1" = _launch ] || exit 2
if [ "$2" = --supported ]; then exit ${supported ? 0 : 1}; fi
printf '%s\\n' "$@" > "${join(dir, 'launch-args')}"
shift 3
MEKO_LAUNCHED=1 exec "$(sed -n 3p "${join(dir, 'launch-args')}")" "$@"
`);
  chmodSync(meko, 0o755);
}

function presenter(env: Record<string, string>, ...args: string[]) {
  return presenterAs('me_presenter', env, ...args);
}

function presenterAs(command: string, env: Record<string, string>, ...args: string[]) {
  const r = spawnSync('sh', [join(dir, 'bin', command), ...args], {
    encoding: 'utf8',
    // HOME and XDG_DATA_HOME in the test folder, so the machine's own meko is never found.
    env: { ...process.env, PATH: `${join(dir, 'meko-bin')}:/usr/bin:/bin`, HOME: join(dir, 'home'), XDG_DATA_HOME: join(dir, 'home', 'share'), MEKO_LAUNCHED: '', ...env },
  });
  expect(r.status, r.stderr).toBe(0);
  return { ran: JSON.parse(r.stdout) as { version: string; args: string[]; appDir: string; launcher: string; launched: string | null }, stderr: r.stderr };
}
const launchArgs = () => existsSync(join(dir, 'launch-args')) ? readFileSync(join(dir, 'launch-args'), 'utf8').trim().split('\n') : null;

describe('the release launcher', () => {
  it('runs the bundled server of the release app/current names, naming its folder and itself', () => {
    fakeMeko(true);
    const { ran } = presenter({}, 'status');
    expect(ran).toEqual({ version: NEW, args: ['status'], appDir: join(app, NEW), launcher: join(app, NEW, 'bin', 'me_presenter'), launched: '' });
    // Run by its name before (an older Meko's registry), it's the same program.
    expect(presenterAs('meko_presenter', {}, 'status').ran.launcher).toBe(join(app, NEW, 'bin', 'me_presenter'));
    // Only serve goes through Meko.
    expect(launchArgs()).toBeNull();
  });

  it('hands serve to Meko\'s launcher, which runs it again', () => {
    fakeMeko(true);
    const { ran, stderr } = presenter({}, 'serve');
    expect(launchArgs()).toEqual(['_launch', 'presenter', join(app, NEW, 'bin', 'me_presenter'), 'serve']);
    expect([ran.args, ran.launched, stderr]).toEqual([['serve'], '1', '']);
  });

  it.each([
    ['a meko too old for _launch', true],
    ['no meko at all', false],
  ])('runs serve itself, saying a failed update won\'t roll back, with %s', (_n, withMeko) => {
    if (withMeko) fakeMeko(false);
    const { ran, stderr } = presenter({}, 'serve');
    expect(ran.args).toEqual(['serve']);
    expect(launchArgs()).toBeNull();
    expect(stderr).toContain('no meko with _launch was found, so a failed update won\'t roll back');
  });

  it('runs serve itself when Meko started it', () => {
    fakeMeko(true);
    const { ran, stderr } = presenter({ MEKO_LAUNCHED: '1' }, 'serve');
    expect([ran.args, launchArgs(), stderr]).toEqual([['serve'], null, '']);
  });
});
