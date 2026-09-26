import { describe, expect, it } from 'vitest';
import * as selfsigned from 'selfsigned';
import { expiresSoon } from '../../src/server/tls.js';

describe('self-signed certificate renewal', () => {
  const certValidFor = async (days: number) =>
    (await selfsigned.generate([{ name: 'commonName', value: 'test' }], { keySize: 2048, notAfterDate: new Date(Date.now() + days * 24 * 3600 * 1000) })).cert;

  it('renews a certificate that expires within 30 days, or can\'t be read', async () => {
    expect(expiresSoon(await certValidFor(400))).toBe(false);
    expect(expiresSoon(await certValidFor(10))).toBe(true);
    expect(expiresSoon('not a certificate')).toBe(true);
  });
});
