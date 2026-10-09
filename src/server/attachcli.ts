import { isTerminalReport } from '../shared/termreports.js';
import { NoHostError, SessionHost, type PaneInfo } from './hostclient.js';
import type { Paths } from './paths.js';

/** Ctrl+\ detaches; nothing else is held back from the session. */
const DETACH = 0x1c;
/** Turns off what the session may have turned on in this terminal (mouse, paste, focus, alt screen). */
const RESTORE = '\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[?1004l\x1b[?1l\x1b>\x1b[?25h\x1b[?1049l\x1b[0m';

/** The pane a name means: its exact name, or the one whose name ends in `--<name>` (a session's slug). */
export function findPane(panes: Map<string, PaneInfo>, name: string): string | null {
  if (panes.has(name)) return name;
  const matches = [...panes.keys()].filter((p) => p.endsWith(`--${name}`));
  return matches.length === 1 ? matches[0] : null;
}

/**
 * `agent-k-local attach [session]`: shows a session's terminal in this one, alongside any
 * browser. With no name, lists the sessions. Ctrl+\ detaches; Claude keeps running.
 */
export async function runAttach(paths: Paths, args: string[]): Promise<void> {
  const host = new SessionHost(paths, { services: () => null });
  let panes: Map<string, PaneInfo>;
  try {
    panes = await host.panes();
  } catch (e) {
    throw e instanceof NoHostError ? new Error('No session host is running, so there is no session to attach to.') : e;
  }
  const name = args.find((a) => !a.startsWith('-'));
  if (!name) {
    if (!panes.size) {
      console.log('No sessions are running.');
      return;
    }
    console.log('Sessions (attach with: agent-k-local attach <name>):');
    for (const p of [...panes.values()].sort((a, b) => a.session.localeCompare(b.session))) {
      console.log(`  ${p.session}${p.dead ? '  (Claude exited)' : ''}`);
    }
    return;
  }
  const pane = findPane(panes, name);
  if (!pane) throw new Error(`No session named ${name}. Run agent-k-local attach to list them.`);
  const { stdin, stdout } = process;
  if (!stdin.isTTY || !stdout.isTTY) throw new Error('attach needs a terminal.');

  const stream = await host.attach(pane, { cols: stdout.columns, rows: stdout.rows });
  await new Promise<void>((resolve) => {
    let done = false;
    const finish = (msg: string) => {
      if (done) return;
      done = true;
      stdin.off('data', onKey);
      stdout.off('resize', onResize);
      stdin.setRawMode(false);
      stdin.pause();
      stream.close();
      stdout.write(`${RESTORE}\r\n${msg}\r\n`);
      resolve();
    };
    const onKey = (d: Buffer) => {
      // This terminal's answers to Claude's queries: the host has answered them already.
      if (isTerminalReport(d.toString('utf8'))) return;
      const at = d.indexOf(DETACH);
      if (at >= 0) {
        if (at > 0) stream.write(d.subarray(0, at).toString('utf8'));
        finish(`Detached from ${pane}. Claude keeps running.`);
        return;
      }
      stream.write(d.toString('utf8'));
    };
    const onResize = () => stream.resize(stdout.columns, stdout.rows);
    stream.onData((d) => stdout.write(d));
    stream.onControl((m) => {
      if (m.type === 'exit') finish(m.reason === 'stopped' ? 'The session host stopped.' : `The session ${pane} ended.`);
    });
    stream.onClose(() => finish(`Disconnected from ${pane}.`));
    stdin.setRawMode(true);
    stdin.resume();
    stdin.on('data', onKey);
    stdout.on('resize', onResize);
  });
}
