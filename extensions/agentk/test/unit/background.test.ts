import { describe, expect, it } from 'vitest';
import { backgroundTasks } from '../../src/server/sessions.js';

describe("a turn end's background tasks", () => {
  it.each<[string, unknown, ReturnType<typeof backgroundTasks>]>([
    ['none sent, as by a Claude Code too old to', undefined, []],
    ['an empty list', [], []],
    ['not a list', { type: 'shell' }, []],
    ['a shell, by its command', [{ id: 'b1', type: 'shell', status: 'running', description: 'Run the tests', command: 'npm test' }], [{ type: 'shell', description: 'npm test' }]],
    ['a subagent, by its description', [{ id: 'a1', type: 'subagent', status: 'running', description: 'Review the change', agent_type: 'general-purpose' }],
      [{ type: 'subagent', description: 'Review the change' }]],
    ['a subagent with no description, by its type', [{ type: 'subagent', description: ' ', agent_type: 'Explore' }], [{ type: 'subagent', description: 'Explore' }]],
    ['one with nothing to say about it', [{ id: 'm1', type: 'monitor' }, {}], [{ type: 'monitor', description: 'monitor' }, { type: 'task', description: 'task' }]],
    ['a command over lines', [{ type: 'shell', command: 'npm run build &&\n  npm test' }], [{ type: 'shell', description: 'npm run build && npm test' }]],
    ['entries that are not tasks', [null, 'npm test', 3], []],
  ])('%s', (_, raw, tasks) => {
    expect(backgroundTasks(raw)).toEqual(tasks);
  });

  it('keeps ten, each a line of at most 200 characters', () => {
    const tasks = backgroundTasks(Array.from({ length: 12 }, (_, i) => ({ type: 'shell', command: `${i} ${'x'.repeat(300)}` })));
    expect(tasks).toHaveLength(10);
    expect(tasks[0].description).toHaveLength(200);
    expect(tasks[0].description.endsWith('…')).toBe(true);
  });
});
