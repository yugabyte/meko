import { randomBytes } from 'node:crypto';
import { chmodSync, renameSync, rmSync, writeFileSync } from 'node:fs';

/**
 * Replaces an executable script with a new file renamed over it. Sessions start Kode's hook and
 * guard scripts at any time, including while the server restarts and rewrites them, and Linux
 * refuses to open a file for writing while it's being exec'd (ETXTBSY); a rename leaves that exec
 * alone. A failed write leaves no temp file behind.
 */
export function writeScript(path: string, text: string): void {
  const tmp = `${path}.${randomBytes(4).toString('hex')}.tmp`;
  try {
    writeFileSync(tmp, text);
    chmodSync(tmp, 0o755);
    renameSync(tmp, path);
  } catch (e) {
    rmSync(tmp, { force: true });
    throw e;
  }
}
