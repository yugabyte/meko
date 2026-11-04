import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { TranscriptCapture, findMekoCaptureScript, mekoPluginInstalls, mekoPluginKeys } from '../../src/server/capture.js';
import { silentLogger } from '../../src/server/log.js';
import { FAKE_CAPTURE, readJsonl } from '../helpers.js';

function registry(claudeDir: string, plugins: Record<string, unknown>): void {
  mkdirSync(join(claudeDir, 'plugins'), { recursive: true });
  writeFileSync(join(claudeDir, 'plugins', 'installed_plugins.json'), JSON.stringify({ version: 2, plugins }));
}

describe('findMekoCaptureScript', () => {
  it('finds capture.js in the installed meko-agent-skills plugin', () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-capture-'));
    const install = join(dir, 'cache', 'meko');
    mkdirSync(join(install, 'hooks-handlers', 'lib'), { recursive: true });
    writeFileSync(join(install, 'hooks-handlers', 'lib', 'capture.js'), '');
    registry(dir, {
      'other@x': [{ installPath: join(dir, 'nope') }],
      'meko-agent-skills@meko-agent-skills': [{ scope: 'user', installPath: install }],
    });
    expect(mekoPluginInstalls(dir)).toEqual([install]);
    expect(findMekoCaptureScript(dir)).toBe(join(install, 'hooks-handlers', 'lib', 'capture.js'));
  });

  it('is null when the plugin, its script, or the registry is missing', () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-capture-'));
    expect(mekoPluginInstalls(dir)).toBeNull();
    expect(findMekoCaptureScript(dir)).toBeNull();
    registry(dir, { 'meko-agent-skills@meko-agent-skills': [{ installPath: join(dir, 'gone') }] });
    expect(findMekoCaptureScript(dir)).toBeNull();
    registry(dir, {});
    expect(findMekoCaptureScript(dir)).toBeNull();
  });
});

describe('mekoPluginKeys', () => {
  it("names the Meko plugin's keys in the registry, and the installer's key even without one", () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-capture-'));
    expect(mekoPluginKeys(dir)).toEqual(['meko-agent-skills@meko-agent-skills']);
    registry(dir, { 'other@x': [], 'meko-agent-skills@team-fork': [], 'meko-agent-skills@meko-agent-skills': [] });
    expect(mekoPluginKeys(dir).sort()).toEqual(['meko-agent-skills@meko-agent-skills', 'meko-agent-skills@team-fork']);
  });
});

describe('TranscriptCapture', () => {
  let dir: string;
  let transcript: string;
  let log: string;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'akl-capture-'));
    transcript = join(dir, 't.jsonl');
    writeFileSync(transcript, '{}\n');
    log = join(dir, 'capture.jsonl');
    process.env.FAKE_CAPTURE_LOG = log;
  });
  afterEach(() => {
    delete process.env.FAKE_CAPTURE_LOG;
    delete process.env.FAKE_CAPTURE_MS;
  });
  const runs = () => readJsonl(log);

  it('runs one capture per session at a time and folds requests made during a run into one more', async () => {
    process.env.FAKE_CAPTURE_MS = '300';
    const c = new TranscriptCapture(FAKE_CAPTURE, () => ({}), silentLogger);
    const first = c.capture('s1', transcript);
    await new Promise((r) => setTimeout(r, 50));
    const a = c.capture('s1', transcript);
    const b = c.capture('s1', transcript);
    await Promise.all([first, a, b]);
    const r = runs();
    expect(r).toHaveLength(2);
    expect(r[1].start).toBeGreaterThanOrEqual(r[0].end);
  });

  it('settles once every capture has finished, including ones started while it waits', async () => {
    const c = new TranscriptCapture(FAKE_CAPTURE, () => ({}), silentLogger);
    await c.settled(5000);
    process.env.FAKE_CAPTURE_MS = '200';
    void c.capture('s1', transcript);
    const done = c.settled(5000);
    await new Promise((r) => setTimeout(r, 100));
    void c.capture('s2', transcript); // ends after s1's
    await done;
    expect(runs()).toHaveLength(2);
  });

  it('stops waiting for captures after the time given', async () => {
    process.env.FAKE_CAPTURE_MS = '1000';
    const c = new TranscriptCapture(FAKE_CAPTURE, () => ({}), silentLogger);
    const run = c.capture('s1', transcript);
    const start = Date.now();
    await c.settled(100);
    expect(Date.now() - start).toBeLessThan(500);
    expect(runs()).toEqual([]);
    await run;
  });

  it('does nothing without a script or a transcript', async () => {
    await new TranscriptCapture(null, () => ({}), silentLogger).capture('s1', transcript);
    await new TranscriptCapture(FAKE_CAPTURE, () => ({}), silentLogger).capture('s1', join(dir, 'missing.jsonl'));
    expect(runs()).toEqual([]);
  });

  it('swallows script failures', async () => {
    const bad = join(dir, 'bad.mjs');
    writeFileSync(bad, 'process.exit(3);\n');
    const warnings: string[] = [];
    const c = new TranscriptCapture(bad, () => ({}), { ...silentLogger, warn: (m) => warnings.push(m) });
    await expect(c.capture('s1', transcript)).resolves.toBeUndefined();
    expect(warnings).toEqual([expect.stringMatching(/capture failed for s1/)]);
  });
});
