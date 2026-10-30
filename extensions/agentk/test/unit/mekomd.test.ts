import { describe, expect, it } from 'vitest';
import { isGotchasDatapack, parseMekoMd } from '../../src/shared/mekomd.js';

const ID1 = '00000000-0000-0000-0000-000000000001';
const ID2 = '00000000-0000-0000-0000-000000000002';

describe('MEKO.md', () => {
  it('reads every table row with an id, by its headers, backticks dropped', () => {
    const text = `# Meko datapacks — demo-repo

Search the ones matching your task.

| Datapack | ID | What's in it |
|---|---|---|
| \`adlc/proj/demo-repo/api\` | \`${ID1}\` | The default for the API service. |
| \`adlc/repo/demo-repo/gotchas\` | \`${ID2}\` | Gotchas mined from history. |
| \`no-id\` | | Skipped. |
| \`adlc/proj/demo-repo/again\` | \`${ID1}\` | A second row for the same id. |

| ID | Datapack |
|:--|:--|
| ${ID2.replace('2', '3')} | Old one |
`;
    expect(parseMekoMd(text)).toEqual([
      { name: 'adlc/proj/demo-repo/api', id: ID1, about: 'The default for the API service.' },
      { name: 'adlc/repo/demo-repo/gotchas', id: ID2, about: 'Gotchas mined from history.' },
      { name: 'Old one', id: ID2.replace('2', '3'), about: '' },
    ]);
  });

  it('reads nothing from a file without a datapack table', () => {
    expect(parseMekoMd('# Notes\n\n| a | b |\n|---|---|\n| 1 | 2 |\n')).toEqual([]);
  });

  it.each([
    ['adlc/repo/demo-repo/gotchas', true],
    ['adlc/repo/demo-repo/gotchas-old', false],
    ['gotchas', false],
  ])('%s is a gotchas datapack: %s', (name, want) => {
    expect(isGotchasDatapack(name)).toBe(want);
  });
});
