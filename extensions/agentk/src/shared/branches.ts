/** What the branch prefix setting puts your GitHub login in place of. */
export const LOGIN_PLACEHOLDER = '{login}';

/** New projects' branch prefix: `ak_<your GitHub login>/`. */
export const DEFAULT_BRANCH_PREFIX = `ak_${LOGIN_PLACEHOLDER}/`;

/** The prefix when one made of little but {login} comes to nothing without a login: a branch is never just its slug. */
const NO_LOGIN_PREFIX = 'ak/';

/**
 * The prefix a new session's branch gets: the project's setting with {login} filled in with the
 * login `gh` is signed in as (lowercased). Without one (gh missing or signed out), {login} is left
 * out with the `_`, `-` or `.` before it, so `ak_{login}/` is `ak/` and `team/{login}/` is
 * `team/`; a prefix that comes to nothing (`{login}/`) is `ak/`.
 */
export function expandBranchPrefix(setting: string, login: string | null): string {
  const user = (login ?? '').toLowerCase().replace(/[^a-z0-9._-]+/g, '-');
  if (user) return setting.split(LOGIN_PLACEHOLDER).join(user);
  const left = setting.split(LOGIN_PLACEHOLDER).map((part, i, parts) => (i < parts.length - 1 ? part.replace(/[_.-]$/, '') : part)).join('');
  return left.replace(/\/{2,}/g, '/').replace(/^[/_.-]+/, '') || NO_LOGIN_PREFIX;
}
