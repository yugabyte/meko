import { describe, expect, it } from 'vitest';
import { formatStatus, headline, upFor, type Paint, type StatusFacts } from '../../src/server/statusview.js';

const NOW = Date.parse('2026-11-03T12:00:00.000Z');

/** A running AgentK, its server up 2h4m, with `o` changed. */
function facts(o: Partial<StatusFacts> = {}): StatusFacts {
  return {
    version: '0.20261103.13',
    probe: { ok: true, version: '0.20261103.13', pid: 39527, startedAt: '2026-11-03T09:56:00.000Z' },
    leasePid: 39527,
    host: { up: true, pid: 39531, sessions: 0, running: 0 },
    services: { kind: 'launchd', units: [
      { id: 'com.yugabyte.me_kode', installed: true, running: true, text: 'running' },
      { id: 'com.yugabyte.me_kode.host', installed: true, running: true, text: 'running' },
    ] },
    webUrl: 'http://127.0.0.1:7777/ext/agentk/',
    home: '/home/alice/.meko/extensions/agentk',
    logs: '/home/alice/.meko/extensions/agentk/logs/server.log',
    now: NOW,
    ...o,
  };
}

describe("meko kode status, laid out as meko status", () => {
  it('prints a row per part, the headline, the links as plain rows, then what to run next', () => {
    expect(formatStatus(facts())).toEqual([
      '  ✓ Server          healthy',
      '  ✓ Session host    running (pid 39531), 0 sessions',
      '  ✓ Service         launchd',
      '',
      '  ● AgentK 0.20261103.13 is running · pid 39527, up 2h4m0s',
      '',
      '  Web UI  http://127.0.0.1:7777/ext/agentk/',
      '  Home    /home/alice/.meko/extensions/agentk',
      '  Logs    /home/alice/.meko/extensions/agentk/logs/server.log',
      '',
      'Next:',
      '  meko kode ui    open AgentK',
    ]);
  });

  it('says when it is stopped, with the sessions the host keeps, and how to start it', () => {
    const lines = formatStatus(facts({
      probe: { ok: false, error: 'connect ENOENT' },
      leasePid: null,
      host: { up: true, pid: 39531, sessions: 2, running: 1 },
      services: { kind: 'systemd', units: [
        { id: 'me_kode.service', installed: true, running: false, text: 'inactive' },
        { id: 'me_kode-host.service', installed: true, running: false, text: 'inactive' },
      ] },
    }));
    expect(lines.slice(0, 4)).toEqual([
      '  ✓ Session host    running (pid 39531), 2 sessions (1 running)',
      '  ! Service         systemd, stopped',
      '',
      "  ✗ AgentK 0.20261103.13 isn't running",
    ]);
    expect(lines.some((l) => l.includes('Server'))).toBe(false);
    expect(lines.slice(-2)).toEqual(['Next:', '  meko kode start    start AgentK']);
  });

  it('starts with the headline Meko reads off a terminal, for a Meko from before MEKO_SUMMARY', () => {
    // A row first would give an older Meko its mark: a stopped server with its host up read as ✓.
    const stopped = facts({ probe: { ok: false, error: 'connect ENOENT' }, leasePid: null });
    expect(formatStatus(stopped, undefined, false).slice(0, 4)).toEqual([
      "✗ AgentK 0.20261103.13 isn't running",
      '',
      '  ✓ Session host    running (pid 39531), 0 sessions',
      '  ✓ Service         launchd',
    ]);
    expect(formatStatus(facts(), undefined, false).slice(2)).toEqual(formatStatus(facts()));
  });

  it('opens with the headline when there are no rows', () => {
    const lines = formatStatus(facts({ probe: { ok: false, error: 'connect ENOENT' }, leasePid: null, host: { up: false }, services: null }));
    expect(lines[0]).toBe("  ✗ AgentK 0.20261103.13 isn't running");
  });

  it('warns about a server that holds the lease but is slow to answer, and a host that is down', () => {
    const lines = formatStatus(facts({ probe: { ok: false, error: 'timed out' }, host: { up: false } }));
    expect(lines).toContain('  ! Server          not answering (timed out)');
    expect(lines).toContain('  ! Session host    not running');
    expect(lines).toContain("  ! AgentK 0.20261103.13 isn't answering · pid 39527");
    expect(lines.slice(-3)).toEqual([
      'Next:',
      "  meko kode logs       see why it isn't answering",
      '  meko kode restart    restart its server',
    ]);
  });

  it('goes without the uptime from a server before it, and says what is wrong with the services', () => {
    const lines = formatStatus(facts({
      probe: { ok: true, version: '0.20261103.12' },
      host: { error: 'EACCES' },
      services: { kind: 'launchd', units: [
        { id: 'com.yugabyte.me_kode', installed: true, running: true, text: 'running' },
        { id: 'com.yugabyte.me_kode.host', installed: false, running: false, text: 'not installed' },
      ] },
      webUrl: null,
      webWhy: "Meko isn't running: start it with meko start",
    }));
    expect(lines).toContain('  ● AgentK 0.20261103.13 is running · pid 39527');
    expect(lines).toContain('  ! Session host    not answering (EACCES)');
    expect(lines).toContain('  ! Service         launchd: com.yugabyte.me_kode.host not installed');
    expect(lines).toContain("  Web UI  none (Meko isn't running: start it with meko start)");
    expect(formatStatus(facts({ services: 'other' }))).toContain('  ! Service         none for this home (the installed services run another one)');
    expect(formatStatus(facts({ services: null })).some((l) => l.includes('Service'))).toBe(false);
  });

  it("colors by Meko's roles: marks, a bold headline with a dim note, dim labels, the link, and the call to action", () => {
    const tag = (t: string) => (s: string) => `<${t}>${s}</${t}>`;
    const paint: Paint = { color: true, mark: (k, m) => `<${k}>${m}</${k}>`, bold: tag('b'), dim: tag('d'), link: tag('a'), cta: tag('cta'), blue: tag('blue') };
    const lines = formatStatus(facts(), paint);
    expect(lines[0]).toBe('  <ok>✓</ok> Server          healthy');
    expect(lines).toContain('  <ok>●</ok> <b>AgentK 0.20261103.13 is running</b><d> · pid 39527, up 2h4m0s</d>');
    expect(lines).toContain('  <d>Web UI</d>  <a>http://127.0.0.1:7777/ext/agentk/</a>');
    expect(lines).toContain('  <d>Home</d>    /home/alice/.meko/extensions/agentk');
    // The call to action starts a column early, so its text lines up as without color.
    expect(lines.at(-1)).toBe(' <cta>meko kode ui</cta>   open AgentK');
  });
});

describe("the headline meko status shows in its AgentK row (MEKO_SUMMARY=1)", () => {
  it('is one marked line, never colored, as before', () => {
    expect(headline(facts())).toBe('✓ AgentK 0.20261103.13 is running (pid 39527, up 2h4m0s)');
    expect(headline(facts({ probe: { ok: false, error: 'timed out' } }))).toBe("! AgentK 0.20261103.13 is running (pid 39527), but isn't answering");
    expect(headline(facts({ probe: { ok: false, error: 'connect ENOENT' }, leasePid: null }))).toBe("✗ AgentK 0.20261103.13 isn't running");
    expect(headline(facts({ probe: { ok: true, version: '0.20261103.12' } }))).toBe('✓ AgentK 0.20261103.13 is running (pid 39527)');
  });
});

describe('upFor', () => {
  it('writes the uptime as Go writes a duration, as meko status does', () => {
    expect(upFor(33_000)).toBe('33s');
    expect(upFor(243_000)).toBe('4m3s');
    expect(upFor((2 * 3600 + 4 * 60) * 1000)).toBe('2h4m0s');
    expect(upFor(-5)).toBe('0s');
  });
});
