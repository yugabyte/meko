import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  LoginLimiter, base32Decode, base32Encode, consumeSetupToken, hashPassword, issueSetupToken, totpCode, totpVerify, verifyPassword,
} from '../../src/server/auth.js';
import { DEFAULT_CONFIG, loadConfig, mekoWebUrl, resolveMekoWebUrl, saveConfig } from '../../src/server/config.js';
import { openDb, run, schemaVersion } from '../../src/server/db.js';
import { githubRepo, prettyRemote } from '../../src/server/git.js';
import { ciState, isLogin } from '../../src/server/github.js';
import { pinSlug, projectAgentId } from '../../src/server/meko.js';
import { getPaths } from '../../src/server/paths.js';
import { render } from '../../src/server/prompts.js';
import { slugify } from '../../src/server/store.js';
import { shq } from '../../src/server/claude.js';
import { mekoDatapackUrl } from '../../src/web/util.js';
import { STAGES, STAGE_LABELS } from '../../src/shared/types.js';

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

  it('keeps Meko transcript capture off unless config.toml turns it on', () => {
    expect(DEFAULT_CONFIG.meko.captureTranscripts).toBe(false);
    const paths = getPaths(mkdtempSync(join(tmpdir(), 'akl-cfg-')));
    const cfg = structuredClone(DEFAULT_CONFIG);
    cfg.meko.captureTranscripts = true;
    saveConfig(paths, cfg);
    expect(readFileSync(paths.config, 'utf8')).toContain('capture_transcripts = true');
    expect(loadConfig(paths).meko.captureTranscripts).toBe(true);
  });

  it('finds the Meko web app for known MCP hosts only', () => {
    expect(mekoWebUrl('https://mcp.mekodata.ai/mcp')).toBe('https://cloud.mekodata.ai');
    expect(mekoWebUrl('https://MCP.mekodata.ai/mcp/')).toBe('https://cloud.mekodata.ai');
    expect(mekoWebUrl('https://mcp.mekodev.com/mcp')).toBe('https://app.mekodev.com');
    expect(mekoWebUrl('http://localhost:8000/mcp')).toBeNull();
    expect(mekoWebUrl('not a url')).toBeNull();
    expect(mekoWebUrl('http://localhost:8000/mcp', ' https://meko.example.com/ ')).toBe('https://meko.example.com');
    const cfg = structuredClone(DEFAULT_CONFIG);
    cfg.meko.webUrl = 'https://meko.internal';
    expect(resolveMekoWebUrl(cfg)).toBe('https://meko.internal');
  });

  it('links a datapack by id', () => {
    expect(mekoDatapackUrl('https://cloud.mekodata.ai', 'eb5ba952-643a-4610-a662-a54a39c82a90')).toBe(
      'https://cloud.mekodata.ai/datapacks/eb5ba952-643a-4610-a662-a54a39c82a90',
    );
    expect(mekoDatapackUrl(null, 'eb5ba952')).toBeNull();
  });

  it('migrates a fresh database', () => {
    const db = openDb(':memory:');
    expect(schemaVersion(db)).toBeGreaterThan(0);
  });

  it('lets several projects share a repo path', () => {
    const db = openDb(':memory:');
    const add = (id: string) =>
      run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES (?, ?, '/code/api', 'main', 0)`, id, id);
    add('api');
    expect(() => add('api-tools')).not.toThrow();
  });

  it('moves sessions left in the old Cleanup stage to Merge & Close', () => {
    const file = join(mkdtempSync(join(tmpdir(), 'ak-db-')), 'state.db');
    let db = openDb(file);
    const version = schemaVersion(db);
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('api', 'api', '/code/api', 'main', 0)`);
    const add = (id: string, stage: string) =>
      run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token, desired, status, stage, created_at)
        VALUES (?, 'api', ?, ?, ?, 'b', 'main', '/w', ?, 't', 'running', 'idle', ?, 0)`, id, id, id, id, id, stage);
    add('s1', 'cleanup');
    add('s2', 'review');
    // Replay the last migration, as on a host upgrading from the six-stage release.
    db.exec(`PRAGMA user_version = ${version - 1}`);
    db.close();
    db = openDb(file);
    expect(schemaVersion(db)).toBe(version);
    const stages = db.prepare('SELECT id, stage FROM sessions ORDER BY id').all();
    expect(stages).toEqual([{ id: 's1', stage: 'merge' }, { id: 's2', stage: 'review' }]);
    db.close();
  });
});

describe('stages', () => {
  it('ends with one Merge & Close step', () => {
    expect(STAGES).toEqual(['design', 'code', 'pr', 'review', 'merge']);
    expect(STAGE_LABELS.merge).toBe('Merge & Close');
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

describe('date-based versions', () => {
  it('orders 0.YYYYMMDD.N releases correctly', async () => {
    const { compareSemver } = await import('../../src/server/updater.js');
    expect(compareSemver('0.20260924.10', '0.20260924.9')).toBeGreaterThan(0);
    expect(compareSemver('0.20260925.0', '0.20260924.12')).toBeGreaterThan(0);
    expect(compareSemver('v0.20260924.0', '0.20260924.0')).toBe(0);
    expect(compareSemver('0.20260924.0', '0.0.0-dev')).toBeGreaterThan(0);
  });
});

describe('setup link lifetime', () => {
  it('reports an outstanding link until it is used', async () => {
    const { setupTokenOutstanding } = await import('../../src/server/auth.js');
    const db = openDb(':memory:');
    expect(setupTokenOutstanding(db)).toBe(false);
    const t = issueSetupToken(db);
    expect(setupTokenOutstanding(db)).toBe(true);
    consumeSetupToken(db, t);
    expect(setupTokenOutstanding(db)).toBe(false);
  });
});
