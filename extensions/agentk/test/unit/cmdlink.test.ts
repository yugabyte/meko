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

  it("adds agentk beside an install's agent-k-local link from before the rename to AgentK, and only there", () => {
    const paths = getPaths(join(home, '.agent-k-local'));
    const bin = join(home, '.local', 'bin');
    const release = join(paths.app, 'current', 'bin');
    mkdirSync(bin, { recursive: true });
    // Nothing installed by the installer here: a dev server, say.
    expect(linkCommand(paths, home)).toBe(false);

    symlinkSync(join(release, 'agent-k-local'), join(bin, 'agent-k-local'));
    // A release from before the rename has no bin/agentk to link to.
    expect(linkCommand(paths, home)).toBe(false);

    mkdirSync(release, { recursive: true });
    writeFileSync(join(release, 'agentk'), '');
    expect(linkCommand(paths, home)).toBe(true);
    expect(readlinkSync(join(bin, 'agentk'))).toBe(join(release, 'agentk'));
    // Never over something already called agentk.
    expect(linkCommand(paths, home)).toBe(false);
  });
});
