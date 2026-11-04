import { mkdirSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { linkCommand } from '../../src/server/cmdlink.js';
import { getPaths } from '../../src/server/paths.js';
import { tempDir } from '../helpers.js';

describe('linkCommand', () => {
  let home: string;
  beforeEach(() => {
    home = tempDir('akl-cmdlink-');
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  it.each(['agentk', 'agent-k-local'])("adds me_koder beside an install's %s link from before, and only there", (old) => {
    const paths = getPaths(join(home, '.agent-k-local'));
    const bin = join(home, '.local', 'bin');
    const release = join(paths.app, 'current', 'bin');
    mkdirSync(bin, { recursive: true });
    // Nothing installed here: a dev server, say.
    expect(linkCommand(paths, home)).toBe(false);

    symlinkSync(join(release, old), join(bin, old));
    // A release from before the rename has no bin/me_koder to link to.
    expect(linkCommand(paths, home)).toBe(false);

    mkdirSync(release, { recursive: true });
    writeFileSync(join(release, 'me_koder'), '');
    expect(linkCommand(paths, home)).toBe(true);
    expect(readlinkSync(join(bin, 'me_koder'))).toBe(join(release, 'me_koder'));
    // Never over something already called me_koder, and the old link stays (an older Meko may run it).
    expect(linkCommand(paths, home)).toBe(false);
    expect(readlinkSync(join(bin, old))).toBe(join(release, old));
  });

  it("leaves an old name's link to another home alone", () => {
    const paths = getPaths(join(home, '.agent-k-local'));
    const bin = join(home, '.local', 'bin');
    mkdirSync(join(paths.app, 'current', 'bin'), { recursive: true });
    writeFileSync(join(paths.app, 'current', 'bin', 'me_koder'), '');
    mkdirSync(bin, { recursive: true });
    symlinkSync(join(home, 'other', 'app', 'current', 'bin', 'agentk'), join(bin, 'agentk'));
    expect(linkCommand(paths, home)).toBe(false);
  });
});
