import { describe, expect, it } from 'vitest';
import { commitsAfter } from '../../src/server/lifecycle.js';
import { datapackIdsIn, localClone } from '../../src/server/prreview.js';

const repo = (name: string, remote: string | null, upstream: string | null = null, lastTouchedAt = 0) =>
  ({ name, path: `/code/${name}`, remote, upstream, lastTouchedAt });

describe("a PR's local clone", () => {
  it('takes a fork whose upstream is the repo before a clone of it, newest first', () => {
    const clones = [repo('demo', 'github.com/acme/demo', null, 5), repo('hari-demo', 'git@github.com:hari/demo.git', 'https://github.com/acme/demo.git', 1)];
    expect(localClone(clones, 'acme/demo')).toBe('/code/hari-demo');
    expect(localClone([clones[0]], 'ACME/Demo')).toBe('/code/demo');
    expect(localClone([repo('old', 'github.com/acme/demo', null, 1), repo('new', 'github.com/acme/demo', null, 9)], 'acme/demo')).toBe('/code/new');
  });

  it('never matches by name alone', () => {
    expect(localClone([repo('demo', null), repo('demo2', 'github.com/other/demo')], 'acme/demo')).toBeNull();
  });
});

describe('PR descriptions and new commits', () => {
  it("finds the datapack ids in a description's Meko lines, with or without the datapack's name", () => {
    const body = 'Text meko://not-at-line-start\nmeko://0f1e2d3c-1111-4222-8333-444455556666\nmeko-dev://9a8b7c6d-0000\nmeko_conversation_id:abc';
    expect(datapackIdsIn(body)).toEqual(['0f1e2d3c-1111-4222-8333-444455556666', '9a8b7c6d-0000']);
    const named = 'Adds it.\n\ncloud.mekodata.ai://0f1e2d3c-1111-4222-8333-444455556666;adlc/proj/demo/my project \nmeko_conversation_id:abc';
    expect(datapackIdsIn(named)).toEqual(['0f1e2d3c-1111-4222-8333-444455556666']);
    expect(datapackIdsIn('meko://0f1e2d3c-1111 and more')).toEqual([]);
  });

  it('counts the commits after the one reviewed, or null after a force-push', () => {
    expect(commitsAfter(['a', 'b', 'c'], 'a')).toBe(2);
    expect(commitsAfter(['a', 'b', 'c'], 'c')).toBe(0);
    expect(commitsAfter(['x', 'y'], 'a')).toBeNull();
    expect(commitsAfter([], null)).toBeNull();
  });
});
