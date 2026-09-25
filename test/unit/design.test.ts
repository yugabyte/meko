import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MAX_DESIGN_HTML, artifactUrlIn, isArtifactPublish, readArtifactHtml, readDesignFile } from '../../src/server/design.js';
import { DESIGN_FRAME_CSP, designFrameDoc } from '../../src/web/design-frame.js';

describe('design capture', () => {
  let dir: string;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'akl-design-'));
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  it('finds a claude.ai Artifact link in a tool result or a file', () => {
    expect(artifactUrlIn('{"text":"Published https://claude.ai/code/artifact/ab12-cd34. Watching."}')).toBe('https://claude.ai/code/artifact/ab12-cd34');
    expect(artifactUrlIn('see https://claude.ai/artifact/xyz_9')).toBe('https://claude.ai/artifact/xyz_9');
    expect(artifactUrlIn('https://example.com/artifact/1')).toBeNull();
  });

  it('counts only page publishes', () => {
    expect(isArtifactPublish({ file_path: '/tmp/d.html' })).toBe(true);
    expect(isArtifactPublish({ action: 'publish', file_path: '/tmp/d.html' })).toBe(true);
    expect(isArtifactPublish({ action: 'read', url: 'https://claude.ai/artifact/x' })).toBe(false);
    expect(isArtifactPublish({ file_path: '/tmp/logo.png', asset: true })).toBe(false);
    expect(isArtifactPublish({ action: 'list' })).toBe(false);
  });

  it('reads the published page only when it is a reasonably sized .html file', () => {
    writeFileSync(join(dir, 'd.html'), '<h1>x</h1>');
    writeFileSync(join(dir, 'notes.txt'), 'secret');
    writeFileSync(join(dir, 'big.html'), 'x'.repeat(MAX_DESIGN_HTML + 1));
    expect(readArtifactHtml('d.html', dir)).toBe('<h1>x</h1>');
    expect(readArtifactHtml(join(dir, 'd.html'), undefined)).toBe('<h1>x</h1>');
    expect(readArtifactHtml(join(dir, 'notes.txt'), undefined)).toBeNull();
    expect(readArtifactHtml(join(dir, 'big.html'), undefined)).toBeNull();
    expect(readArtifactHtml(join(dir, 'missing.html'), undefined)).toBeNull();
    expect(readArtifactHtml(42, dir)).toBeNull();
  });

  it('ignores a missing or blank design file', () => {
    expect(readDesignFile(join(dir, 'design.md'))).toBeNull();
    writeFileSync(join(dir, 'design.md'), '  \n');
    expect(readDesignFile(join(dir, 'design.md'))).toBeNull();
    writeFileSync(join(dir, 'design.md'), '# D\n');
    expect(readDesignFile(join(dir, 'design.md'))).toBe('# D\n');
  });
});

describe('design frame', () => {
  const meta = `<meta http-equiv="Content-Security-Policy" content="${DESIGN_FRAME_CSP}">`;

  it('puts the policy first in <head>, before any of the page\'s scripts', () => {
    const doc = designFrameDoc('<!doctype html><html lang="en"><head><script>x()</script></head><body></body></html>', null);
    expect(doc).toBe(`<!doctype html><html lang="en"><head>${meta}<script>x()</script></head><body></body></html>`);
    expect(designFrameDoc('<p>bare</p>', null)).toBe(`${meta}<p>bare</p>`);
  });

  it('allows no network requests and no same-origin access', () => {
    expect(DESIGN_FRAME_CSP).toContain("default-src 'none'");
    expect(DESIGN_FRAME_CSP).not.toMatch(/connect-src/);
    expect(DESIGN_FRAME_CSP).not.toMatch(/'self'/);
  });

  it('passes an explicit app theme to the page', () => {
    expect(designFrameDoc('<html><head></head></html>', 'dark')).toMatch(/^<html data-theme="dark"><head>/);
    expect(designFrameDoc('<html lang="en"><head></head></html>', 'light')).toMatch(/^<html data-theme="light" lang="en">/);
    expect(designFrameDoc('<html><head></head></html>', null)).toMatch(/^<html><head>/);
    expect(designFrameDoc('<htmlx></htmlx>', 'dark')).not.toContain('data-theme');
  });
});
