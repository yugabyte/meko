import { X509Certificate } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { hostname, networkInterfaces } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import * as selfsigned from 'selfsigned';
import { isLoopback, type Config } from './config.js';
import { expandHome, type Paths } from './paths.js';

export interface TlsMaterial {
  cert: string;
  key: string;
  fingerprint: string;
  selfSigned: boolean;
}

export function fqdn(): string {
  try {
    const f = execFileSync('hostname', ['-f'], { encoding: 'utf8' }).trim();
    if (f) return f;
  } catch {
    /* fall through */
  }
  return hostname();
}

/** A self-signed pair is made again this long before it expires, so browsers never see it lapse. */
const RENEW_BEFORE_MS = 30 * 24 * 3600 * 1000;

/** True when the PEM is unreadable or expires within `RENEW_BEFORE_MS`. */
export function expiresSoon(certPem: string, now = Date.now()): boolean {
  try {
    return Date.parse(new X509Certificate(certPem).validTo) - now < RENEW_BEFORE_MS;
  } catch {
    return true;
  }
}

/**
 * TLS is mandatory for any non-loopback bind. Uses the configured cert/key,
 * or generates a self-signed pair covering this host's names and IPs: once, and again when it's
 * about to expire (the fingerprint then changes, and the server logs the new one at startup).
 */
export async function loadTls(paths: Paths, cfg: Config): Promise<TlsMaterial | null> {
  if (cfg.tls.cert && cfg.tls.key) {
    const cert = readFileSync(expandHome(cfg.tls.cert), 'utf8');
    return { cert, key: readFileSync(expandHome(cfg.tls.key), 'utf8'), fingerprint: fingerprint(cert), selfSigned: false };
  }
  if (isLoopback(cfg.server.bind)) return null;
  const certFile = join(paths.certs, 'self-signed.crt');
  const keyFile = join(paths.certs, 'self-signed.key');
  if (!existsSync(certFile) || !existsSync(keyFile) || expiresSoon(readFileSync(certFile, 'utf8'))) {
    mkdirSync(paths.certs, { recursive: true, mode: 0o700 });
    const names = [...new Set([fqdn(), hostname(), 'localhost'])];
    const ips = Object.values(networkInterfaces()).flat().filter((i) => i && !i.internal).map((i) => i!.address);
    const altNames = [
      ...names.map((value) => ({ type: 2 as const, value })),
      ...['127.0.0.1', ...ips].map((ip) => ({ type: 7 as const, ip })),
    ];
    const pems = await selfsigned.generate([{ name: 'commonName', value: fqdn() }], {
      keySize: 2048,
      notAfterDate: new Date(Date.now() + 825 * 24 * 3600 * 1000),
      algorithm: 'sha256',
      extensions: [
        { name: 'basicConstraints', cA: false },
        { name: 'keyUsage', digitalSignature: true, keyEncipherment: true },
        { name: 'extKeyUsage', serverAuth: true },
        { name: 'subjectAltName', altNames },
      ],
    });
    writeFileSync(certFile, pems.cert, { mode: 0o644 });
    writeFileSync(keyFile, pems.private, { mode: 0o600 });
  }
  const cert = readFileSync(certFile, 'utf8');
  return { cert, key: readFileSync(keyFile, 'utf8'), fingerprint: fingerprint(cert), selfSigned: true };
}

export function fingerprint(certPem: string): string {
  try {
    return new X509Certificate(certPem).fingerprint256;
  } catch {
    return 'unknown';
  }
}
