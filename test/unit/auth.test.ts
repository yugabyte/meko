import { describe, expect, it } from 'vitest';
import { localAccess } from '../../src/server/auth.js';

describe('local access', () => {
  const local = { host: 'localhost:7777' };

  it('skips the login only for a loopback bind, a loopback peer and a localhost Host', () => {
    expect(localAccess('127.0.0.1', '127.0.0.1', local)).toBe(true);
    expect(localAccess('::1', '::1', { host: '[::1]:7777' })).toBe(true);
    expect(localAccess('localhost', '::ffff:127.0.0.1', { host: '127.0.0.1:7777' })).toBe(true);
    expect(localAccess('127.0.0.1', '127.0.0.1', { host: 'LOCALHOST' })).toBe(true);
  });

  it('never applies to a network bind, another peer, another Host or a forwarded request', () => {
    expect(localAccess('0.0.0.0', '127.0.0.1', local)).toBe(false);
    expect(localAccess('10.1.2.3', '127.0.0.1', local)).toBe(false);
    expect(localAccess('127.0.0.1', '10.1.2.3', local)).toBe(false);
    expect(localAccess('127.0.0.1', '::ffff:10.1.2.3', local)).toBe(false);
    expect(localAccess('127.0.0.1', undefined, local)).toBe(false);
    expect(localAccess('127.0.0.1', '127.0.0.1', {})).toBe(false);
    expect(localAccess('127.0.0.1', '127.0.0.1', { host: 'rebound.example:7777' })).toBe(false);
    expect(localAccess('127.0.0.1', '127.0.0.1', { host: 'localhost.rebound.example' })).toBe(false);
    expect(localAccess('127.0.0.1', '127.0.0.1', { host: 'bad host' })).toBe(false);
    expect(localAccess('127.0.0.1', '127.0.0.1', { ...local, 'x-forwarded-for': '10.1.2.3' })).toBe(false);
    expect(localAccess('127.0.0.1', '127.0.0.1', { ...local, 'x-forwarded-host': 'agentk.example' })).toBe(false);
  });
});
