import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  MAX_DESIGN_HTML, artifactUrlIn, designLine, isArtifactPublish, isDesignLine, mekoDesignName, parseDesignLine, readArtifactHtml, readDesignFile, sha256Hex,
  withDesignLine,
} from '../../src/server/design.js';
import { mekoDatapackLineId } from '../../src/server/prompts.js';
import { MEKO_SOURCE_MARK } from '../../src/shared/decisions.js';
import { DESIGN_FRAME_CSP, MEKO_MARK_DATA_URL, MERMAID_VERSION, designFrameDoc, mekoSourceStyle, mermaidLoader } from '../../src/web/design-frame.js';
import { DIAGRAM_FAILED, ERROR_CHARS, mermaidLook } from '../../src/web/mermaid.js';

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

  it('names each later version of a design in Meko with a v number', () => {
    expect(mekoDesignName('new-login', 'md', 1)).toBe('design-new-login.md');
    expect(mekoDesignName('new-login', 'md', 2)).toBe('design-new-login-v2.md');
    expect(mekoDesignName('new-login', 'html', 12)).toBe('design-new-login-v12.html');
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

describe('designs in PRs', () => {
  const hash = sha256Hex('# Design');
  const line = designLine({ name: 'design-widget.md', sha256: hash });
  const isDp = (l: string) => mekoDatapackLineId(l) !== null;

  it("names the design by its first version's name and the latest version's hash", () => {
    expect(hash).toMatch(/^[0-9a-f]{64}$/);
    expect(line).toBe(`meko_design:design-widget.md;${hash}`);
    expect(parseDesignLine(`Adds it.\n\nmeko://dp-1\n  ${line}  \nmeko_conversation_id:c1`)).toEqual({ name: 'design-widget.md', sha256: hash });
    expect(parseDesignLine(`meko_design:design-widget.html;${hash.toUpperCase()}\r`)).toEqual({ name: 'design-widget.html', sha256: hash });
    // A line that isn't a design's name and a hash names nothing.
    expect(parseDesignLine('meko_design:design-widget.md')).toBeNull();
    expect(parseDesignLine(`meko_design:notes.txt;${hash}`)).toBeNull();
    expect(parseDesignLine(`meko_design:design-widget.md;${hash.slice(1)}`)).toBeNull();
    expect(parseDesignLine('Adds it.')).toBeNull();
    expect(isDesignLine(line)).toBe(true);
    // A mention of the format isn't AgentK's line.
    expect(isDesignLine("      meko_design:<first version's name>;<latest version's hash>, and")).toBe(false);
    expect(isDesignLine('meko_design:anything')).toBe(false);
  });

  it("sets the PR's design line, leaving the rest of the description as it was", () => {
    const old = `meko_design:design-widget.md;${'0'.repeat(64)}`;
    // Replaced where it is, any second one dropped.
    expect(withDesignLine(`Adds it.\n\nmeko://0f1e2d3c-1111\n${old}\nmeko_conversation_id:c1\n${old}`, line, isDp))
      .toBe(`Adds it.\n\nmeko://0f1e2d3c-1111\n${line}\nmeko_conversation_id:c1`);
    // Missing: after the datapack line, keeping a CRLF body's line endings.
    expect(withDesignLine('Adds it.\r\n\r\nmeko://0f1e2d3c-1111;team/x\r\nmeko_conversation_id:c1', line, isDp))
      .toBe(`Adds it.\r\n\r\nmeko://0f1e2d3c-1111;team/x\r\n${line}\r\nmeko_conversation_id:c1`);
    // A mention of the format in the description is left alone.
    const doc = "The PR names it:\n\n    meko_design:<name>;<hash>\n\nmeko://0f1e2d3c-1111";
    expect(withDesignLine(doc, line, isDp)).toBe(`${doc}\n${line}`);
    // No Meko lines at all: at the end.
    expect(withDesignLine('Adds it.\n', line, isDp)).toBe(`Adds it.\n\n${line}`);
    expect(withDesignLine('', line, isDp)).toBe(line);
  });
});

describe('design frame', () => {
  const meta = `<meta http-equiv="Content-Security-Policy" content="${DESIGN_FRAME_CSP}">`;

  it('puts the policy first, before any of the page\'s scripts', () => {
    const doc = designFrameDoc('<!doctype html><html lang="en"><head><script>x()</script></head><body></body></html>', null);
    expect(doc).toBe(`<!doctype html>${meta}<html lang="en"><head><script>x()</script></head><body></body></html>`);
    expect(designFrameDoc('<p>bare</p>', null)).toBe(`<!doctype html>${meta}<p>bare</p>`);
  });

  it('keeps the policy in <head> for fragments with a <header>, a commented <head>, or an early script', () => {
    // Each of these once got the policy placed after the page's own content (or inside <body>).
    for (const page of [
      '<style>h{}</style><header class="top">x</header><script>fetch("//x")</script>',
      '<!-- <head> --><html><head></head><body></body></html>',
      '<script>fetch("//x")</script><head></head><body></body>',
    ]) {
      expect(designFrameDoc(page, null).startsWith(`<!doctype html>${meta}`)).toBe(true);
    }
  });

  it('allows no network requests and no same-origin access', () => {
    expect(DESIGN_FRAME_CSP).toContain("default-src 'none'");
    expect(DESIGN_FRAME_CSP).not.toMatch(/connect-src/);
    expect(DESIGN_FRAME_CSP).not.toMatch(/'self'/);
  });

  it("draws Meko's mark on the design's markers, after the page and only when it has one", () => {
    const page = `<html><body><h3>D1 · One SQLite file ${MEKO_SOURCE_MARK}</h3><pre class="mermaid">flowchart LR\n  a --> b</pre></body></html>`;
    const style = mekoSourceStyle(page);
    expect(style).toMatch(/^<style>span\.meko-source \{/);
    expect(style).toContain(`mask: url("${MEKO_MARK_DATA_URL}")`);
    expect(designFrameDoc(page, null)).toBe(`<!doctype html>${meta}${page}${style}${mermaidLoader(page)}`);
    expect(mekoSourceStyle('<p>bare</p>')).toBe('');
    expect(designFrameDoc('<p>bare</p>', null)).not.toContain('<style>');
  });

  it("carries Meko's m as a data: URL of public/meko-m.png", () => {
    const png = readFileSync(join(import.meta.dirname, '../../src/web/public/meko-m.png'));
    expect(MEKO_MARK_DATA_URL).toBe(`data:image/png;base64,${png.toString('base64')}`);
  });

  it('passes an explicit app theme to the page', () => {
    expect(designFrameDoc('<html><head></head></html>', 'dark')).toBe(`<!doctype html><html data-theme="dark">${meta}<html><head></head></html>`);
    expect(designFrameDoc('<p>fragment</p>', 'light')).toMatch(/^<!doctype html><html data-theme="light"><meta /);
    expect(designFrameDoc('<html><head></head></html>', null)).not.toContain('data-theme');
  });

  it('draws Mermaid blocks a page leaves to the claude.ai viewer', () => {
    const page = '<html><body><pre class="mermaid">flowchart LR\n  a --> b</pre></body></html>';
    const doc = designFrameDoc(page, 'dark');
    expect(doc.startsWith(`<!doctype html><html data-theme="dark">${meta}${page}`)).toBe(true);
    expect(doc.endsWith(`${mermaidLoader(page)}`)).toBe(true);
    const loader = mermaidLoader(page);
    expect(loader).toContain(`https://cdn.jsdelivr.net/npm/mermaid@${MERMAID_VERSION}/dist/mermaid.esm.min.mjs`);
    expect(loader).toContain("securityLevel: 'strict'");
    // The same look as markdown designs: SVG labels, the wider wrap, the system font.
    expect(loader).toContain(JSON.stringify(mermaidLook(true)));
    expect(loader).toContain(JSON.stringify(mermaidLook(false)));
    // jsdelivr is one of the policy's script sources, so the loader needs nothing more.
    expect(DESIGN_FRAME_CSP).toContain('https://cdn.jsdelivr.net');
    // A diagram that names Mermaid's API isn't the page running Mermaid.
    expect(mermaidLoader('<pre class="mermaid">flowchart LR\n  a["mermaid.render()"] --> b</pre>')).not.toBe('');
    for (const block of [
      "<div class='diagram mermaid'>graph TD</div>",
      '<pre class=mermaid>graph TD</pre>',
      '<pre id="a" class="mermaid wide">graph TD</pre>',
      '<PRE CLASS="mermaid">x</PRE>',
    ]) {
      expect(mermaidLoader(block)).not.toBe('');
    }
  });

  it('shows the source of a block that does not draw instead of Mermaid\'s error graphic', () => {
    const loader = mermaidLoader('<pre class="mermaid">sequenceDiagram\n  A->>B: save under pastes/<session>/</pre>');
    // No error graphic, and each block on its own, so one that fails doesn't stop the rest.
    expect(loader).toContain('suppressErrorRendering: true');
    expect(loader).toContain('await mermaid.run({ nodes: [block] });');
    expect(loader).not.toContain("querySelector: '.mermaid'");
    // The source Mermaid read, entities decoded, kept before Mermaid empties the block.
    expect(loader.indexOf('decode.innerHTML = block.innerHTML;')).toBeLessThan(loader.indexOf('await mermaid.run'));
    // A failed block becomes the markdown designs' note, the error and the source, all as text.
    expect(loader).toContain(`note.textContent = ${JSON.stringify(DIAGRAM_FAILED)};`);
    expect(loader).toContain('box.append(note, pre(message(err)), pre(source));');
    // Mermaid's error is cut where markdown designs cut it.
    expect(loader).toContain(`text.length > ${ERROR_CHARS} ? text.slice(0, ${ERROR_CHARS}) + '…' : text`);
    expect(loader).toContain('block.replaceWith(box);');
    // The only HTML it writes is the block's own, into a detached <textarea>, which keeps tags as text.
    expect(loader.match(/\.innerHTML\s*=.*/g)).toEqual(['.innerHTML = block.innerHTML;']);
  });

  it('adds no Mermaid when the page has no blocks or loads Mermaid itself', () => {
    for (const page of [
      '<p>no diagrams</p>',
      '<pre><code class="language-js">mermaid</code></pre>',
      // Only `mermaid` as a whole class token counts, as `.mermaid` selects it.
      '<pre class="language-mermaid">graph TD</pre>',
      '<div class="mermaid-container"><p>x</p></div>',
      '<div data-class="mermaid">x</div>',
      '<pre class="Mermaid">graph TD</pre>',
      '<pre class="mermaid">graph TD</pre><script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>',
      '<pre class="mermaid">graph TD</pre><script type="module">import mermaid from "https://cdn.jsdelivr.net/npm/mermaid/+esm"; mermaid.run();</script>',
      '<pre class="mermaid">graph TD</pre><script>mermaid.initialize({ startOnLoad: true })</script>',
      '<script type="module">const { default: m } = await import("https://cdn.jsdelivr.net/npm/mermaid@11/+esm");</script><div class="mermaid">x</div>',
    ]) {
      expect(mermaidLoader(page)).toBe('');
      expect(designFrameDoc(page, null)).not.toContain('mermaid.esm.min.mjs');
    }
  });

  it('loads the same Mermaid version that is bundled for markdown designs', () => {
    const pkg = JSON.parse(readFileSync(join(process.cwd(), 'node_modules/mermaid/package.json'), 'utf8')) as { version: string };
    expect(mermaidLoader('<pre class="mermaid">graph TD</pre>')).toContain(`/npm/mermaid@${pkg.version}/dist/`);
  });
});
