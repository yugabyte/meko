import { describe, expect, it } from 'vitest';
import { silentLogger } from '../../src/server/log.js';
import { MekoQuestions } from '../../src/server/mekoquestions.js';

describe("Meko's Context Gaps client", () => {
  const local = { url: 'http://127.0.0.1:7778/mcp', token: 'local-token' };

  it('reads every page of open questions, after the last one shown, and none from a Meko that keeps none', async () => {
    const urls: string[] = [];
    const pages = [
      { questions: [{ key: 'a', query: 'A', asked_by_conversation: true, last_at: '2026-10-02T00:00:00Z' }, { key: 'b', query: 'B', last_at: '2026-10-01T00:00:00Z' }], more: true },
      { questions: [{ key: 'c', query: 'C', last_at: '2026-09-30T00:00:00Z' }], more: false },
    ];
    let status = 200;
    const fetch = async (url: string) => {
      urls.push(url);
      return { ok: status === 200, status, json: async () => ({ data: pages[urls.length - 1] }) };
    };
    const q = new MekoQuestions({ meko: () => local, log: silentLogger, fetch });
    expect(await q.openQuestions('dp 1', 'conv-1')).toEqual([{ key: 'a', query: 'A', own: true }, { key: 'b', query: 'B', own: false }, { key: 'c', query: 'C', own: false }]);
    expect(urls).toEqual([
      'http://127.0.0.1:7778/api/v1/datapacks/dp%201/questions?state=open&conversation_id=conv-1',
      'http://127.0.0.1:7778/api/v1/datapacks/dp%201/questions?state=open&conversation_id=conv-1&after=2026-10-01T00%3A00%3A00Z&after_key=b',
    ]);
    // An older Meko (404), and no Meko on this computer in use: no list, and the second asks nothing.
    urls.length = 0;
    status = 404;
    expect(await q.openQuestions('dp', null)).toBeNull();
    expect(await new MekoQuestions({ meko: () => null, log: silentLogger, fetch }).openCounts('dp')).toBeNull();
    expect(urls).toHaveLength(1);
  });
});
