import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, rmSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { writeScript } from '../../src/server/scriptfile.js';
import { tempDir, waitFor } from '../helpers.js';

describe('writeScript', () => {
  let dir: string;
  beforeEach(() => {
    dir = tempDir('akl-script-');
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  it("replaces a script sessions keep starting (Linux refuses a write in place during an exec: ETXTBSY)", async () => {
    const script = join(dir, 'hook.sh');
    writeScript(script, '#!/bin/sh\nexit 0\n');
    const marker = join(dir, 'looping');
    const loop = spawn('/bin/sh', ['-c', `: > "${marker}"; while :; do "${script}"; done`], { stdio: 'ignore' });
    try {
      await waitFor(() => existsSync(marker), Boolean, 5000, 'the loop to start');
      for (let i = 0; i < 200; i++) writeScript(script, `#!/bin/sh\nexit 0 # ${i}\n`);
    } finally {
      loop.kill();
    }
    expect(statSync(script).mode & 0o777).toBe(0o755);
  });

  it('leaves no temp file when it fails', () => {
    const taken = join(dir, 'taken');
    mkdirSync(join(taken, 'not-empty'), { recursive: true }); // a rename can't replace a folder
    expect(() => writeScript(taken, 'x')).toThrow();
    expect(readdirSync(dir)).toEqual(['taken']);
  });
});
