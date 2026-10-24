import { describe, expect, it } from 'vitest';
import { releaseNotes } from '../../src/server/releasenotes.js';

const commit = (message: string, sha = 'abcdef0123') => ({ sha, message, url: `https://github.com/o/r/commit/${sha}` });

describe('releaseNotes', () => {
  it('sorts releasing subjects into sections, breaking changes first, and links PRs', () => {
    const notes = releaseNotes(
      [
        commit('feat(web)!: drop the old layout (#12)', '1111111aaa'),
        commit('fix: stop a crash\n\nBREAKING CHANGE: the config moves', '2222222bbb'),
        commit('deps: bump fastify (#14)', '3333333ccc'),
        commit('perf(server): cache the scan', '4444444ddd'),
        commit('feat: a second feature', '5555555eee'),
      ],
      'o/r',
    );
    expect(notes).toEqual([
      {
        title: 'Breaking changes',
        items: [
          { component: 'web', summary: 'drop the old layout', pr: 12, sha: '1111111', url: 'https://github.com/o/r/pull/12' },
          { component: null, summary: 'the config moves', pr: null, sha: '2222222', url: 'https://github.com/o/r/commit/2222222bbb' },
        ],
      },
      {
        title: 'Features',
        items: [
          { component: 'web', summary: 'drop the old layout', pr: 12, sha: '1111111', url: 'https://github.com/o/r/pull/12' },
          { component: null, summary: 'a second feature', pr: null, sha: '5555555', url: 'https://github.com/o/r/commit/5555555eee' },
        ],
      },
      { title: 'Fixes', items: [{ component: null, summary: 'stop a crash', pr: null, sha: '2222222', url: 'https://github.com/o/r/commit/2222222bbb' }] },
      { title: 'Performance', items: [{ component: 'server', summary: 'cache the scan', pr: null, sha: '4444444', url: 'https://github.com/o/r/commit/4444444ddd' }] },
      { title: 'Dependencies', items: [{ component: null, summary: 'bump fastify', pr: 14, sha: '3333333', url: 'https://github.com/o/r/pull/14' }] },
    ]);
  });

  it.each([
    'docs: explain the update loop',
    'chore(release): v0.20261017.0',
    'nit!: rename a thing\n\nBREAKING CHANGE: not from a type that releases',
    'Merge branch main',
    'feature: not a type',
    // Meko's subjects release Meko, not AgentK.
    'feat(meko/cli): add a command',
    'fix(meko)!: change the store\n\nBREAKING CHANGE: a new format',
  ])('leaves out %j', (message) => {
    expect(releaseNotes([commit(message)], 'o/r')).toEqual([]);
  });

  it.each(['feat(agentk,meko): ship both (#20)', 'feat(meko/cli,agentk/web): ship both (#20)', 'feat(meko,repo): ship both (#20)'])(
    'keeps %j: a subject that names Meko and another component releases both',
    (message) => {
      const component = /\(([^)]*)\)/.exec(message)![1];
      expect(releaseNotes([commit(message, '6666666fff')], 'o/r')).toEqual([
        { title: 'Features', items: [{ component, summary: 'ship both', pr: 20, sha: '6666666', url: 'https://github.com/o/r/pull/20' }] },
      ]);
    },
  );
});
