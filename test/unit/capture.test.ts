import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { TranscriptCapture, capturedConversation, findMekoCaptureScript, mekoPluginInstalls } from '../../src/server/capture.js';
import { silentLogger } from '../../src/server/log.js';
import { FAKE_CAPTURE } from '../helpers.js';

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
  const runs = () => (existsSync(log) ? readFileSync(log, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : []);

  it('runs the script in checkpoint mode with the transcript and Meko credentials', async () => {
    const c = new TranscriptCapture(FAKE_CAPTURE, () => ({ MEKO_MCP_URL: 'https://meko.test/mcp', MEKO_API_KEY: 'k' }), silentLogger);
    await c.capture('s1', transcript);
    expect(runs()).toEqual([expect.objectContaining({
      args: ['checkpoint'],
      input: { transcript_path: transcript },
      env: { MEKO_MCP_URL: 'https://meko.test/mcp', MEKO_API_KEY: 'k' },
    })]);
  });

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

describe('capturedConversation', () => {
  afterEach(() => {
    delete process.env.MEKO_WATERMARK_DIR;
  });

  it("reads the conversation from the plugin's watermark for a transcript", () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-capture-'));
    process.env.MEKO_WATERMARK_DIR = dir;
    expect(capturedConversation(join(dir, 'abc.jsonl'))).toBeNull();
    writeFileSync(join(dir, 'abc.watermark.json'), JSON.stringify({ conversation_id: 'c1', agent_id: 'claude_code:x', datapack_id: null }));
    expect(capturedConversation('/somewhere/abc.jsonl')).toEqual({ conversationId: 'c1', agentId: 'claude_code:x', datapackId: null });
    writeFileSync(join(dir, 'abc.watermark.json'), JSON.stringify({ conversation_id: '', last_line_number: 0 }));
    expect(capturedConversation('/somewhere/abc.jsonl')).toBeNull();
  });
});
