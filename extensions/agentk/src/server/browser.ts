import { spawn } from 'node:child_process';

/**
 * The command that opens a URL in this computer's browser: `open` on macOS, `xdg-open` elsewhere,
 * or null with no display to show one on (an SSH login without X forwarding).
 */
export function browserCommand(platform: NodeJS.Platform = process.platform, env: NodeJS.ProcessEnv = process.env): string | null {
  if (platform === 'darwin') return 'open';
  return env.DISPLAY || env.WAYLAND_DISPLAY ? 'xdg-open' : null;
}

/**
 * Opens `url` with `cmd`, and says whether that worked. An opener that exits non-zero (xdg-open
 * with no browser) failed; one still running after `waitMs` may be the browser itself (xdg-open
 * can run it in the foreground), so it's left running on its own.
 */
export function openBrowser(url: string, cmd: string | null = browserCommand(), waitMs = 2000): Promise<boolean> {
  if (!cmd) return Promise.resolve(false);
  return new Promise((resolve) => {
    const child = spawn(cmd, [url], { detached: true, stdio: 'ignore' });
    const timer = setTimeout(() => {
      child.unref();
      resolve(true);
    }, waitMs);
    child.on('error', () => {
      clearTimeout(timer);
      resolve(false);
    });
    child.on('exit', (code) => {
      clearTimeout(timer);
      resolve(code === 0);
    });
  });
}
