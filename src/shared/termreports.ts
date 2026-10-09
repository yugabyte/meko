// Terminal answers to a program's queries, told apart from typing. Used by the web UI and `agent-k-local attach`.

/**
 * Whether terminal input is the terminal's answer to a query from the program (device attributes, cursor
 * position, mode and status reports, colors, version, keyboard flags) rather than something typed.
 * The session host answers those itself, once, so every viewer's own answers are dropped: a
 * browser's (xterm.js) and, with `agent-k-local attach`, a real terminal's. A modified
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
