import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  LoginLimiter, base32Decode, base32Encode, consumeSetupToken, hashPassword, issueSetupToken, totpCode, totpVerify, verifyPassword,
} from '../../src/server/auth.js';
import { DEFAULT_CONFIG, loadConfig, saveConfig } from '../../src/server/config.js';
import { openDb, schemaVersion } from '../../src/server/db.js';
import { githubRepo, prettyRemote } from '../../src/server/git.js';
import { ciState, isLogin } from '../../src/server/github.js';
import { pinSlug, projectAgentId } from '../../src/server/meko.js';
import { getPaths } from '../../src/server/paths.js';
import { render } from '../../src/server/prompts.js';
import { slugify } from '../../src/server/store.js';
import { shq } from '../../src/server/claude.js';

describe('auth', () => {
  it('hashes and verifies passwords with scrypt', () => {
    const h = hashPassword('correct horse battery');
    expect(h.startsWith('scrypt$')).toBe(true);
    expect(verifyPassword('correct horse battery', h)).toBe(true);
    expect(verifyPassword('wrong', h)).toBe(false);
  });

  it('matches the RFC 6238 TOTP test vector (SHA-1, 6 digits)', () => {
    const secret = base32Encode(Buffer.from('12345678901234567890'));
    expect(secret).toBe('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ');
    expect(base32Decode(secret).toString()).toBe('12345678901234567890');
    expect(totpCode(secret, 59_000)).toBe('287082');
    expect(totpCode(secret, 1_111_111_109_000)).toBe('081804');
    expect(totpVerify(secret, '287082', 59_000 + 30_000)).toBe(true); // one step of drift
    expect(totpVerify(secret, '287082', 59_000 + 120_000)).toBe(false);
  });

  it('setup tokens are single use', () => {
    const db = openDb(':memory:');
    const t = issueSetupToken(db);
    expect(consumeSetupToken(db, 'nope')).toBe(false);
    expect(consumeSetupToken(db, t)).toBe(true);
    expect(consumeSetupToken(db, t)).toBe(false);
  });

  it('limits failed logins per IP', () => {
    const l = new LoginLimiter(3, 1000);
    for (let i = 0; i < 3; i++) l.fail('1.2.3.4', 0);
    expect(l.blocked('1.2.3.4', 10)).toBe(true);
    expect(l.blocked('5.6.7.8', 10)).toBe(false);
    expect(l.blocked('1.2.3.4', 2000)).toBe(false);
  });
});

describe('config and db', () => {
  it('round-trips config.toml with snake_case keys and keeps defaults', () => {
    const paths = getPaths(mkdtempSync(join(tmpdir(), 'akl-cfg-')));
    const cfg = structuredClone(DEFAULT_CONFIG);
    cfg.repos.roots = ['~/src', '~/code'];
    cfg.update.channel = 'prerelease';
    saveConfig(paths, cfg);
    const back = loadConfig(paths);
    expect(back.repos.roots).toEqual(['~/src', '~/code']);
    expect(back.update.channel).toBe('prerelease');
    expect(back.server.port).toBe(7777);
  });

  it('migrates a fresh database', () => {
    const db = openDb(':memory:');
    expect(schemaVersion(db)).toBeGreaterThan(0);
  });
});

describe('naming', () => {
  it('slugifies like Meko agent ids', () => {
    expect(slugify('Fix the Retry Backoff!!')).toBe('fix-the-retry-backoff');
    expect(slugify('agent_k.local')).toBe('agent-k-local');
    expect(projectAgentId('billing-api')).toBe('claude_code:billing-api');
    expect(pinSlug('claude_code:billing-api')).toBe('claude_code_billing-api');
  });

  it('normalizes git remotes', () => {
    expect(prettyRemote('git@github.com:acme/api.git')).toBe('github.com/acme/api');
    expect(prettyRemote('https://user@github.com/acme/api.git')).toBe('github.com/acme/api');
    expect(githubRepo('git@github.com:acme/api.git')).toBe('acme/api');
    expect(githubRepo('ssh://phorge.example/diffusion/X')).toBeNull();
  });

  it('quotes shell arguments safely', () => {
    expect(shq("it's")).toBe(`'it'\\''s'`);
  });

  it('renders prompt placeholders', () => {
    expect(render('PR #{{pr}} on {{branch}}{{missing}}', { pr: 12, branch: 'ak/x' })).toBe('PR #12 on ak/x');
  });
});

describe('github helpers', () => {
  it('summarizes CI state', () => {
    expect(ciState(null)).toBe('none');
    expect(ciState([{ status: 'COMPLETED', conclusion: 'SUCCESS' }, { state: 'SUCCESS' }])).toBe('success');
    expect(ciState([{ status: 'IN_PROGRESS', conclusion: '' }])).toBe('pending');
    expect(ciState([{ status: 'COMPLETED', conclusion: 'FAILURE' }, { status: 'IN_PROGRESS' }])).toBe('failure');
    expect(ciState([{ status: 'COMPLETED', conclusion: 'SKIPPED' }, { status: 'COMPLETED', conclusion: 'NEUTRAL' }])).toBe('success');
  });

  it('matches bot logins with or without [bot]', () => {
    expect(isLogin({ login: 'yb-agentk-dev[bot]' }, 'yb-agentk-dev')).toBe(true);
    expect(isLogin({ login: 'yb-agentk-dev' }, 'yb-agentk-dev[bot]')).toBe(true);
    expect(isLogin({ login: 'someone' }, 'yb-agentk-dev')).toBe(false);
    expect(isLogin(null, 'yb-agentk-dev')).toBe(false);
  });
});

describe('Claude folder trust', () => {
  it('trusts a folder once and recognizes trusted parents', async () => {
    const { ensureWorkspaceTrust, isTrusted } = await import('../../src/server/trust.js');
    const { writeFileSync, readFileSync, mkdtempSync: mk } = await import('node:fs');
    const dir = mk(join(tmpdir(), 'akl-trust-'));
    process.env.CLAUDE_CONFIG_DIR = dir;
    writeFileSync(join(dir, '.claude.json'), JSON.stringify({ projects: { '/home/u': { hasTrustDialogAccepted: true } }, other: 1 }));
    expect(ensureWorkspaceTrust('/home/u/.agent-k-local/worktrees')).toBe('already');
    expect(ensureWorkspaceTrust('/data/ak/worktrees')).toBe('added');
    const cfg = JSON.parse(readFileSync(join(dir, '.claude.json'), 'utf8'));
    expect(cfg.other).toBe(1);
    expect(isTrusted('/data/ak/worktrees/p/s', cfg)).toBe(true);
    expect(ensureWorkspaceTrust('/data/ak/worktrees')).toBe('already');
  });
});
