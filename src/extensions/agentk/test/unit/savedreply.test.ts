import { mkdirSync, mkdtempSync, realpathSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { readSavedReplySync, savedReplyFile } from '../../src/server/savedreply.js';

const notice = (file: string) => `Error: result (90,000 characters) exceeds maximum allowed tokens. Output has been saved to ${file}\nFormat: JSON with schema: {result: string}`;

describe("Claude Code's saved replies", () => {
  const setup = () => {
    const dir = mkdtempSync(join(tmpdir(), 'akl-saved-'));
    const main = join(dir, 'c.jsonl');
    writeFileSync(main, '');
    mkdirSync(join(dir, 'c', 'tool-results'), { recursive: true });
    mkdirSync(join(dir, 'other', 'tool-results'), { recursive: true });
    const own = join(dir, 'c', 'tool-results', 'mcp-meko-context_search-1.txt');
    writeFileSync(own, '{"result":"{}"}');
    return { dir, main, own };
  };

  it("reads a reply only from the session's own tool-results folder", () => {
    const { dir, main, own } = setup();
    expect(readSavedReplySync(notice(own), main)).toBe('{"result":"{}"}');
    // A notice ending its sentence with a period names the file without it. The file comes back by its
    // real path (macOS's temp folder, /var/folders, is a link into /private/var).
    expect(savedReplyFile(`${notice(own).split('\n')[0]}.`, main)).toBe(realpathSync(own));
    // Another session's folder, a file beside the transcript, one that doesn't exist, or other text: none.
    const theirs = join(dir, 'other', 'tool-results', 'x.txt');
    writeFileSync(theirs, 'theirs');
    const beside = join(dir, 'x.txt');
    writeFileSync(beside, 'beside');
    expect(readSavedReplySync(notice(theirs), main)).toBeNull();
    expect(readSavedReplySync(notice(beside), main)).toBeNull();
    expect(readSavedReplySync(notice(join(dir, 'c', 'tool-results', 'gone.txt')), main)).toBeNull();
    expect(readSavedReplySync('{"result":"{}"}', main)).toBeNull();
    // A link in the folder that leads out of it is followed, and refused.
    const link = join(dir, 'c', 'tool-results', 'link.txt');
    symlinkSync(theirs, link);
    expect(readSavedReplySync(notice(link), main)).toBeNull();
  });

  it('leaves a reply over the cap unread', () => {
    const { main, own } = setup();
    writeFileSync(own, 'x'.repeat(100));
    expect(readSavedReplySync(notice(own), main, 99)).toBeNull();
    expect(readSavedReplySync(notice(own), main, 100)).toBe('x'.repeat(100));
  });
});
