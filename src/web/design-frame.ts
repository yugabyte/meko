// A published Artifact's HTML, prepared for the design panel's sandboxed frame.
//
// The frame is sandboxed without allow-same-origin, so the page runs in an opaque origin:
// it can't read AgentK Local's cookies or call its API. The policy below also limits what it
// can load to what Artifacts themselves may use (inline code, the two script CDNs, Google
// Fonts, data: and blob: media), with no network requests of its own.

import { mermaidLook } from './mermaid';

export const DESIGN_FRAME_CSP = [
  "default-src 'none'",
  "script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net",
  "style-src 'unsafe-inline' https://fonts.googleapis.com",
  'font-src https://fonts.gstatic.com data:',
  'img-src data: blob:',
  'media-src data: blob:',
].join('; ');

/**
 * The Mermaid version the frame loads from jsdelivr. It matches the one bundled for markdown
 * designs (a test checks), and the frame uses the same look (`mermaidLook`), so a diagram looks
 * the same in both.
 */
export const MERMAID_VERSION = '12.1.0';

/**
 * A <pre> or <div> with the mermaid class (a whole class token, as `.mermaid` matches it: not
 * `language-mermaid` or `mermaid-container`), which the claude.ai viewer draws as a diagram.
 */
const MERMAID_BLOCK =
  /<(?:pre|div|PRE|DIV)\b[^>]*\s(?:class|CLASS)\s*=\s*(?:"(?:[^"]*\s)?mermaid(?:\s[^"]*)?"|'(?:[^']*\s)?mermaid(?:\s[^']*)?'|mermaid(?=[\s/>]))/;
/** A <script> element, its tag and its code. */
const SCRIPT = /<script\b[^>]*>[\s\S]*?(?:<\/script\s*>|$)/gi;
/** A script that loads or runs Mermaid (looked for only in scripts: a diagram may name it too). */
const OWN_MERMAID = /^<script\b[^>]*\bsrc\s*=[^>]*mermaid|\bfrom\s*["'][^"']*mermaid|\bimport\s*\(\s*["'][^"']*mermaid|\bmermaid\s*\.\s*(?:initialize|run|init|render)\s*\(/i;

/**
 * The script that draws a page's Mermaid blocks, for a page that has them but leaves drawing them
 * to the claude.ai viewer; empty otherwise. Mermaid comes from jsdelivr, which the policy allows,
 * in strict mode, themed like the frame.
 */
export function mermaidLoader(html: string): string {
  if (!MERMAID_BLOCK.test(html) || (html.match(SCRIPT) ?? []).some((script) => OWN_MERMAID.test(script))) return '';
  return `<script type="module">
import mermaid from 'https://cdn.jsdelivr.net/npm/mermaid@${MERMAID_VERSION}/dist/mermaid.esm.min.mjs';
const theme = document.documentElement.getAttribute('data-theme');
const dark = theme ? theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
const look = dark ? ${JSON.stringify(mermaidLook(true))} : ${JSON.stringify(mermaidLook(false))};
mermaid.initialize({ ...look, startOnLoad: false, securityLevel: 'strict' });
await mermaid.run({ querySelector: '.mermaid' });
</script>`;
}

/**
 * The frame's srcdoc: the policy as the very first element, the app's theme applied, and
 * Mermaid loaded when the page relies on the viewer for its diagrams.
 *
 * The policy never goes after anything of the page's own. A CSP <meta> only counts inside
 * <head>, and only for what comes after it, so inserting it after the page's `<head>` breaks on
 * a fragment with no <head> but a <header>, a `<head>` inside a comment, or a script before
 * <head>. Putting it first always works: a <meta> before any tag opens <html> and <head>
 * itself; the page's own <html> then only adds attributes, and its <head> and doctype are
 * ignored while their contents still land in <head>.
 */
export function designFrameDoc(html: string, theme: string | null): string {
  const meta = `<meta http-equiv="Content-Security-Policy" content="${DESIGN_FRAME_CSP}">`;
  // Artifacts theme themselves from prefers-color-scheme, overridden by data-theme on <html>.
  // Ours comes first, so it wins over a data-theme the page sets itself.
  const open = theme === 'light' || theme === 'dark' ? `<html data-theme="${theme}">` : '';
  // A policy in the page itself can only narrow this one, never widen it.
  return `<!doctype html>${open}${meta}${html.replace(/^\uFEFF?\s*<!doctype[^>]*>/i, '')}${mermaidLoader(html)}`;
}
