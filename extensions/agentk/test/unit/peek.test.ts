import { describe, expect, it } from 'vitest';
import { lastActivity } from '../../src/server/peek.js';

const at = '2026-10-27T10:00:00.000Z';
const said = (text: string, extra = {}) => JSON.stringify({ type: 'assistant', timestamp: at, message: { content: [{ type: 'text', text }] }, ...extra });
const ran = (name: string, input: object) => JSON.stringify({ type: 'assistant', timestamp: at, message: { content: [{ type: 'tool_use', name, input }] } });
const user = JSON.stringify({ type: 'user', message: { content: 'go on' } });

describe("a session's latest, from its transcript", () => {
  it.each([
    ['the last reply, and no tool when none came after it', [ran('Bash', { command: 'ls' }), said('All done.'), user], 'All done.', null],
    ['the tool called after the reply, by what it does', [said('Looking.'), ran('Bash', { command: 'npm   test', description: 'Run the tests' })], 'Looking.', ['Bash', 'Run the tests']],
    ["subagents' and meta lines left out, and a bad line skipped", [said('Mine.'), said('Sub.', { isSidechain: true }), 'not json', said('Meta.', { isMeta: true })], 'Mine.', null],
    ['no reply yet', [user, ran('Read', { file_path: '/a/b.ts' })], null, ['Read', '/a/b.ts']],
  ])('reads %s', (_what, lines, reply, tool) => {
    const r = lastActivity(lines);
    expect(r.reply?.text ?? null).toBe(reply);
    expect(r.tool ? [r.tool.name, r.tool.summary] : null).toEqual(tool);
  });

  it('cuts a long reply', () => {
    expect(lastActivity([said('x'.repeat(5000))]).reply!.text).toHaveLength(4000);
  });
});
