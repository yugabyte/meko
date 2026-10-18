import { describe, expect, it } from 'vitest';
import { newRepoNameProblem } from '../../src/shared/types.js';

describe('newRepoNameProblem', () => {
  it('takes one plain folder name', () => {
    for (const n of ['fresh', 'my-repo', 'my_repo.v2', '_x', '9lives', ' padded ', 'x'.repeat(100)]) expect(newRepoNameProblem(n), n).toBeNull();
  });

  it('says what is wrong with anything else', () => {
    expect(newRepoNameProblem('')).toBe('Name the new repo.');
    expect(newRepoNameProblem('   ')).toBe('Name the new repo.');
    expect(newRepoNameProblem('x'.repeat(101))).toMatch(/100 characters/);
    for (const n of ['../x', 'a/b', 'a b', 'ünï', 'a\\b']) expect(newRepoNameProblem(n), n).toMatch(/Use only letters/);
    for (const n of ['.', '..', '.git', '-x']) expect(newRepoNameProblem(n), n).toMatch(/Start the repo name/);
  });
});
