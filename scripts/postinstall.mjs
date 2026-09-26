// npm postinstall: node-pty's macOS prebuilds ship spawn-helper without its execute bit, so
// every pty spawn fails with "posix_spawnp failed" (dev and CI on macOS; release builds compile
// node-pty from source and drop the prebuilds). Restore the bit.
import { chmodSync, existsSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

const prebuilds = join('node_modules', 'node-pty', 'prebuilds');
if (existsSync(prebuilds)) {
  for (const dir of readdirSync(prebuilds)) {
    const helper = join(prebuilds, dir, 'spawn-helper');
    if (existsSync(helper)) chmodSync(helper, 0o755);
  }
}
