import { execFile } from 'node:child_process';

/** The version `<command> --version` prints (`2.1.287 (Claude Code)` → `2.1.287`), or null. */
export function cliVersion(command: string): Promise<string | null> {
  return new Promise((resolve) => {
    execFile(command, ['--version'], { timeout: 10_000, env: { ...process.env, NO_COLOR: '1' } }, (err, stdout) => {
      resolve(err ? null : (/(\d+\.\d+\.\d+)/.exec(String(stdout))?.[1] ?? null));
    });
  });
}

/** Whether dotted version `v` is `min` or later (numeric parts; `2.1.10` is after `2.1.9`). */
export function versionAtLeast(v: string, min: string): boolean {
  const a = v.split('.').map(Number);
  const b = min.split('.').map(Number);
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const x = a[i] || 0;
    const y = b[i] || 0;
    if (x !== y) return x > y;
  }
  return true;
}
