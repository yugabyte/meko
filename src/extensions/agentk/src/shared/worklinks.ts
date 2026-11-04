// Links on work items: GitHub PRs and issues, Jira issues, Slack messages, other pages. Used by the server and the web UI.
import { parseGithubRef } from './github-refs.js';
import type { WorkLink } from './types.js';

const JIRA_KEY = /^[A-Z][A-Z0-9_]+-\d{1,7}$/;
const JIRA_PATH = /\/browse\/([A-Z][A-Z0-9_]+-\d{1,7})(?:[/?#]|$)/;
const SLACK_PATH = /^\/archives\/([A-Z0-9]+)\/p(\d{10,20})(?:[/?#]|$)/;

function asUrl(s: string): URL | null {
  if (!/^https?:\/\//i.test(s)) return null;
  try {
    return new URL(s);
  } catch {
    return null;
  }
}

/**
 * A link as typed or pasted: a GitHub PR or issue (a link or `owner/name#N`), a Jira issue (a
 * `/browse/KEY-N` or `?selectedIssue=KEY-N` link on any site, or the bare key), a Slack message
 * (`<workspace>.slack.com/archives/<channel>/p<ts>`), or any other http(s) link. Null for anything else.
 * Links to the same PR, issue or message share a key, however they're spelled.
 */
export function parseWorkLink(input: string): WorkLink | null {
  const s = input.trim();
  if (!s || /\s/.test(s)) return null;
  const gh = parseGithubRef(s);
  if (gh) {
    const url = asUrl(s) ? s : `https://github.com/${gh.repo}/${gh.kind === 'pr' ? 'pull' : 'issues'}/${gh.number}`;
    return { kind: 'github', key: `gh:${gh.repo.toLowerCase()}#${gh.number}`, url, label: `${gh.repo.split('/')[1]}#${gh.number}` };
  }
  if (JIRA_KEY.test(s)) return { kind: 'jira', key: `jira:${s}`, url: null, label: s };
  const u = asUrl(s);
  if (!u) return null;
  const host = u.hostname.toLowerCase();
  const slack = host.endsWith('.slack.com') ? SLACK_PATH.exec(u.pathname) : null;
  if (slack) return { kind: 'slack', key: `slack:${slack[1]}/p${slack[2]}`, url: s, label: 'Slack' };
  const jira = JIRA_PATH.exec(u.pathname)?.[1] ?? u.searchParams.get('selectedIssue');
  if (jira && JIRA_KEY.test(jira)) return { kind: 'jira', key: `jira:${jira}`, url: s, label: jira };
  return { kind: 'other', key: `url:${u.href}`, url: s, label: host.replace(/^www\./, '') };
}

/** The link the palette looks work items up by: one parseWorkLink reads as a GitHub, Jira or Slack link. */
export function workLinkKey(input: string): string | null {
  const l = parseWorkLink(input);
  return l && l.kind !== 'other' ? l.key : null;
}
