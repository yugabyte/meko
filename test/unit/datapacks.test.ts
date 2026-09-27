import { describe, expect, it } from 'vitest';
import {
  isProjectDatapack, matchRepo, normaliseRemote, parseProjectDatapack, projectDatapackDescription, remoteFromDescription, remoteRepoName,
  shortRemote, withoutCredentials,
} from '../../src/shared/datapacks.js';

describe('project datapack names', () => {
  it('splits adlc/proj/<repo>/<project>', () => {
    expect(parseProjectDatapack('adlc/proj/yugabyte-db/vector-tenant-id')).toEqual({ repo: 'yugabyte-db', project: 'vector-tenant-id' });
    expect(parseProjectDatapack('adlc/proj/agentk/agent_core')).toEqual({ repo: 'agentk', project: 'agent_core' });
  });

  it('keeps slashes in the project part', () => {
    expect(parseProjectDatapack('adlc/proj/billing-api/team/retry')).toEqual({ repo: 'billing-api', project: 'team/retry' });
  });

  it('refuses other names', () => {
    expect(parseProjectDatapack('adlc/repo/agent-k-local')).toBeNull();
    expect(parseProjectDatapack('team/kb')).toBeNull();
    expect(parseProjectDatapack('adlc/proj/only-repo')).toBeNull();
    expect(parseProjectDatapack('adlc/proj//x')).toBeNull();
    expect(parseProjectDatapack('adlc/proj/repo/')).toBeNull();
    expect(isProjectDatapack('adlc/proj/a/b')).toBe(true);
    expect(isProjectDatapack('adlc/project/a/b')).toBe(false);
  });
});

describe('remotes', () => {
  it('normalises SSH, HTTPS and short forms to one value', () => {
    const want = 'github.com/corp/billing-api';
    expect(normaliseRemote('git@github.com:corp/billing-api.git')).toBe(want);
    expect(normaliseRemote('https://github.com/corp/billing-api')).toBe(want);
    expect(normaliseRemote('https://user@GitHub.com/Corp/billing-api.git/')).toBe(want);
    expect(normaliseRemote('ssh://git@github.com:22/corp/billing-api.git')).toBe(want);
    expect(normaliseRemote('github.com/corp/billing-api')).toBe(want);
  });

  it('refuses what is not a remote, local paths included', () => {
    expect(normaliseRemote('')).toBeNull();
    expect(normaliseRemote('billing-api')).toBeNull();
    expect(normaliseRemote('file:///tmp/x')).toBeNull();
    expect(normaliseRemote('file://host/tmp/x')).toBeNull();
    expect(normaliseRemote('../billing')).toBeNull();
    expect(normaliseRemote('src/billing.git')).toBeNull();
    expect(normaliseRemote('/home/alice/src/billing.git')).toBeNull();
    expect(projectDatapackDescription('p', '../billing')).toBe('AgentK Local project "p"');
    expect(remoteFromDescription('AgentK Local project "p" · repo /home/alice/x.git')).toBeNull();
  });

  it('takes the repo name from the end', () => {
    expect(remoteRepoName('git@github.com:corp/billing-api.git')).toBe('billing-api');
    expect(remoteRepoName('github.com/corp/ledger')).toBe('ledger');
  });

  it('drops credentials but keeps a plain SSH user', () => {
    expect(withoutCredentials('https://x-access-token:ghp_abc@github.com/corp/a.git')).toBe('https://github.com/corp/a.git');
    expect(withoutCredentials('https://ghp_abc@github.com/corp/a.git')).toBe('https://github.com/corp/a.git');
    expect(withoutCredentials('ssh://me:pw@host/corp/a.git')).toBe('ssh://host/corp/a.git');
    expect(withoutCredentials('ssh://git@host/corp/a.git')).toBe('ssh://git@host/corp/a.git');
    expect(withoutCredentials('git@github.com:corp/a.git')).toBe('git@github.com:corp/a.git');
    expect(projectDatapackDescription('p', 'https://u:tok@github.com/corp/a.git')).toBe('AgentK Local project "p" · repo https://github.com/corp/a.git');
    expect(remoteFromDescription('AgentK Local project "p" · repo https://u:tok@github.com/corp/a.git')).toBe('https://github.com/corp/a.git');
  });

  it('shortens remotes the way the repo list shows them, keeping case', () => {
    expect(shortRemote('git@github.com:Corp/API.git')).toBe('github.com/Corp/API');
    expect(shortRemote('https://user:tok@github.com/corp/api.git/')).toBe('github.com/corp/api');
    expect(shortRemote('/srv/git/local.git')).toBe('/srv/git/local');
  });

  it('round-trips the remote through the description', () => {
    const d = projectDatapackDescription('retry-backoff', 'git@github.com:corp/billing-api.git');
    expect(d).toBe('AgentK Local project "retry-backoff" · repo git@github.com:corp/billing-api.git');
    expect(remoteFromDescription(d)).toBe('git@github.com:corp/billing-api.git');
    expect(remoteFromDescription(projectDatapackDescription('x', null))).toBeNull();
    expect(remoteFromDescription('Some other text · repo git@x:y/z')).toBeNull();
    expect(remoteFromDescription(null)).toBeNull();
  });
});

describe('matchRepo', () => {
  const repo = (name: string, remote: string | null, lastTouchedAt: number, path = `/code/${name}`) => ({ name, path, remote, lastTouchedAt });

  it('prefers the repo whose origin is the remote from the description', () => {
    const repos = [repo('billing-api', 'github.com/other/billing-api', 9), repo('billing', 'github.com/corp/billing-api', 1)];
    const m = matchRepo(repos, 'adlc/proj/billing-api/retry', 'git@github.com:corp/billing-api.git');
    expect(m).toMatchObject({ by: 'remote', count: 1 });
    expect(m.repo?.name).toBe('billing');
  });

  it('falls back to the folder name', () => {
    const m = matchRepo([repo('ledger', null, 1), repo('Billing_API', null, 2)], 'adlc/proj/billing-api/retry', null);
    expect(m.by).toBe('name');
    expect(m.repo?.name).toBe('Billing_API');
  });

  it('matches a renamed folder by its remote name', () => {
    const m = matchRepo([repo('agent-k-local-2', 'github.com/yugabyte/agentk', 1)], 'adlc/proj/agentk/agent_core', null);
    expect(m.repo?.name).toBe('agent-k-local-2');
  });

  it('picks the most recently touched of several and counts them', () => {
    const m = matchRepo([repo('api', null, 1, '/a/api'), repo('api', null, 5, '/b/api')], 'adlc/proj/api/x', null);
    expect(m).toMatchObject({ by: 'name', count: 2 });
    expect(m.repo?.path).toBe('/b/api');
  });

  it('finds nothing for other names or no match', () => {
    expect(matchRepo([repo('api', null, 1)], 'adlc/proj/ledger/x', null)).toEqual({ repo: null, by: null, count: 0 });
    expect(matchRepo([repo('api', null, 1)], 'team/api', null).repo).toBeNull();
  });
});
