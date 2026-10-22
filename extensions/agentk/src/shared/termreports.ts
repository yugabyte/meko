// Terminal answers to a program's queries, told apart from typing (the web UI and `agentk attach`),
// and reports sent without anyone using the screen (the server's one-screen-per-session rule).

/**
 * Whether terminal input is the terminal's answer to a query from the program (device attributes, cursor
 * position, mode and status reports, colors, version, keyboard flags) rather than something typed.
 * The session host answers those itself, once, so every viewer's own answers are dropped: a
 * browser's (xterm.js) and, with `agentk attach`, a real terminal's. A modified
 * F3 (`ESC [1;2R` … `ESC [1;8R`) reads like a cursor position report and is kept.
 */
export function isTerminalReport(data: string): boolean {
  /* eslint-disable no-control-regex */
  return (
    /^\x1b\[[?>][\d;]*c$/.test(data) || // device attributes
    (/^\x1b\[\d+;\d+R$/.test(data) && !/^\x1b\[1;[2-8]R$/.test(data)) || // cursor position
    /^\x1b\[\??\d+(;\d+)*\$y$/.test(data) || // mode report
    /^\x1b\[[0-3]n$/.test(data) || // status
    /^\x1b\[\?\d+u$/.test(data) || // keyboard flags
    /^\x1b\[\d+;\d+;\d+t$/.test(data) || // window size
    /^\x1b\][\d;]+[^\x07\x1b]*(\x07|\x1b\\)$/.test(data) || // OSC answers (colors)
    /^\x1bP[\s\S]*\x1b\\$/.test(data) // DCS answers (version, settings)
  );
  /* eslint-enable no-control-regex */
}

/**
 * Whether terminal input is a report the terminal sends without anyone using it: mouse motion with no
 * button held (SGR or X10 encoding: the motion bit set and the button bits 3) and focus in or out.
 * Moving the pointer across a window isn't using that screen, so these don't keep a session on it
 * (src/server/screens.ts). A drag, motion with a button held, is use.
 */
export function isPassiveReport(data: string): boolean {
  if (data === '\x1b[I' || data === '\x1b[O') return true;
  const sgr = /^\x1b\[<(\d+);\d+;\d+[Mm]$/.exec(data); // eslint-disable-line no-control-regex
  const button = sgr ? Number(sgr[1]) : data.length === 6 && data.startsWith('\x1b[M') ? data.charCodeAt(3) - 32 : -1;
  return button >= 0 && (button & 32) !== 0 && (button & 3) === 3;
}
