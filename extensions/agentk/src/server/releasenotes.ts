import type { ReleaseNoteItem, ReleaseNoteSection } from '../shared/types.js';

/**
 * The commit types that release, in the order their sections show: the same list as `RELEASE` in
 * the repo root's scripts/release/release-notes.sh and scripts/check-title.sh
 * (test/unit/check-title.test.ts keeps them in step).
 */
export const RELEASE_TYPES = ['feat', 'fix', 'perf', 'deps'] as const;

const TITLES: Record<(typeof RELEASE_TYPES)[number], string> = {
  feat: 'Features',
  fix: 'Fixes',
  perf: 'Performance',
  deps: 'Dependencies',
};

export interface NoteCommit {
  sha: string;
  /** The whole commit message: the subject, then the body. */
  message: string;
  url: string;
}

const SUBJECT = new RegExp(`^(${RELEASE_TYPES.join('|')})(?:\\(([^)]*)\\))?(!?): (.+)$`);
const PR = /\s*\(#(\d+)\)$/;
/**
 * A component that isn't AgentK's: a subject that names only such components (Meko's, which release
 * Meko, and Presenter's, which release nothing yet) isn't AgentK's, and one that names one in a list
 * with AgentK's (feat(agentk,meko): ...) is (`exclude` in scripts/release/product.sh).
 */
const OTHER_PRODUCT = /^(meko|presenter)(\/|$)/;

/**
 * Release notes for a range of commits, newest first, by the rules of
 * scripts/release/release-notes.sh --product agentk: only feat, fix, perf and deps subjects count,
 * Meko's and Presenter's (subjects naming only `meko` or `presenter`) don't, a `!` or a `BREAKING CHANGE:` footer on one of them also lists
 * it under Breaking changes, and a section with nothing in it is left out.
 * `repo` (owner/name) links a subject's "(#123)" to its PR.
 */
export function releaseNotes(commits: NoteCommit[], repo: string): ReleaseNoteSection[] {
  const breaking: ReleaseNoteItem[] = [];
  const byType = new Map<string, ReleaseNoteItem[]>(RELEASE_TYPES.map((t) => [t, []]));
  for (const c of commits) {
    const [subject = '', ...body] = c.message.split('\n');
    const m = SUBJECT.exec(subject.trim());
    if (!m) continue;
    const [, type, component, bang, rest] = m as unknown as [string, string, string | undefined, string, string];
    if (component && component.split(',').every((c) => OTHER_PRODUCT.test(c))) continue;
    const pr = PR.exec(rest);
    const item: ReleaseNoteItem = {
      component: component || null,
      summary: pr ? rest.slice(0, pr.index) : rest,
      pr: pr ? Number(pr[1]) : null,
      sha: c.sha.slice(0, 7),
      url: pr ? `https://github.com/${repo}/pull/${pr[1]}` : c.url,
    };
    byType.get(type)!.push(item);
    if (bang) breaking.push(item);
    for (const line of body) {
      if (line.startsWith('BREAKING CHANGE: ')) breaking.push({ ...item, component: null, summary: line.slice(17).trim() });
    }
  }
  const sections = [{ title: 'Breaking changes', items: breaking }, ...RELEASE_TYPES.map((t) => ({ title: TITLES[t], items: byType.get(t)! }))];
  return sections.filter((s) => s.items.length);
}
