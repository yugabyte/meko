import { describe, expect, it } from 'vitest';
import { DEFAULT_BRANCH_PREFIX, expandBranchPrefix } from '../../src/shared/branches.js';

describe('branch prefix', () => {
  it('fills {login} in with the GitHub login, lowercased', () => {
    expect(DEFAULT_BRANCH_PREFIX).toBe('ak_{login}/');
    expect(expandBranchPrefix(DEFAULT_BRANCH_PREFIX, 'Hari90')).toBe('ak_hari90/');
    expect(expandBranchPrefix('{login}/ak/', 'octo-cat')).toBe('octo-cat/ak/');
    // Anything git wouldn't take in a branch name becomes a dash.
    expect(expandBranchPrefix('ak_{login}/', 'a b~c')).toBe('ak_a-b-c/');
  });

  it('leaves {login} out, with what joins it, when gh has no login', () => {
    expect(expandBranchPrefix(DEFAULT_BRANCH_PREFIX, null)).toBe('ak/');
    expect(expandBranchPrefix('ak-{login}/', '')).toBe('ak/');
    expect(expandBranchPrefix('{login}-ak/', null)).toBe('ak/');
    // A path-style prefix keeps one slash, which git needs.
    expect(expandBranchPrefix('team/{login}/', null)).toBe('team/');
    expect(expandBranchPrefix('team/{login}/', 'me')).toBe('team/me/');
  });

  it('falls back to ak/ when the prefix comes to nothing without a login', () => {
    expect(expandBranchPrefix('{login}/', null)).toBe('ak/');
    expect(expandBranchPrefix('{login}', null)).toBe('ak/');
    expect(expandBranchPrefix('{login}/', 'me')).toBe('me/');
  });

  it('keeps a prefix without {login} as it is', () => {
    expect(expandBranchPrefix('ak/', 'me')).toBe('ak/');
    expect(expandBranchPrefix('feature/', null)).toBe('feature/');
  });
});
