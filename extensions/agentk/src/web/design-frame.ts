// A published Artifact's HTML, prepared for the design panel's sandboxed frame.
//
// The frame is sandboxed without allow-same-origin, so the page runs in an opaque origin:
// it can't read AgentK Local's cookies or call its API. The policy below also limits what it
// can load to what Artifacts themselves may use (inline code, the two script CDNs, Google
// Fonts, data: and blob: media), with no network requests of its own.

import { MEKO_SOURCE_CLASS } from '../shared/decisions';
import { DIAGRAM_FAILED, ERROR_CHARS, mermaidLook } from './mermaid';

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
 *
 * Blocks are drawn one at a time, so one that doesn't draw is replaced by a note, Mermaid's error
 * and its source (as text), not Mermaid's error graphic, and the rest still draw. The source is
 * what Mermaid read, not what the page's HTML says: the browser has already parsed the block, so
 * a `<session>` in a label shows up as the tags it became, matching the error.
 */
export function mermaidLoader(html: string): string {
  if (!MERMAID_BLOCK.test(html) || (html.match(SCRIPT) ?? []).some((script) => OWN_MERMAID.test(script))) return '';
  return `<script type="module">
import mermaid from 'https://cdn.jsdelivr.net/npm/mermaid@${MERMAID_VERSION}/dist/mermaid.esm.min.mjs';
const theme = document.documentElement.getAttribute('data-theme');
const dark = theme ? theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
const look = dark ? ${JSON.stringify(mermaidLook(true))} : ${JSON.stringify(mermaidLook(false))};
mermaid.initialize({ ...look, startOnLoad: false, securityLevel: 'strict', suppressErrorRendering: true });
const message = (err) => {
  const text = String(err?.message ?? err?.str ?? err).trim() || 'unknown error';
  return text.length > ${ERROR_CHARS} ? text.slice(0, ${ERROR_CHARS}) + '…' : text;
};
const pre = (text) => {
  const el = document.createElement('pre');
  el.style.cssText = 'white-space: pre-wrap; overflow-wrap: anywhere';
  el.textContent = text;
  return el;
};
for (const block of document.querySelectorAll('.mermaid')) {
  const decode = document.createElement('textarea');
  decode.innerHTML = block.innerHTML;
  const source = decode.value.trim();
  try {
    await mermaid.run({ nodes: [block] });
  } catch (err) {
    const note = document.createElement('p');
    note.textContent = ${JSON.stringify(DIAGRAM_FAILED)};
    const box = document.createElement('div');
    box.className = 'mermaid-error';
    box.append(note, pre(message(err)), pre(source));
    block.replaceWith(box);
  }
}
</script>`;
}

/** Meko's "m" (`public/meko-m.png`) as a data: URL, which the frame's policy allows (a test checks the bytes). */
export const MEKO_MARK_DATA_URL =
  'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEgAAABICAYAAABV7bNHAAAFl0lEQVR42u2aT2xUVRjFz7l3Skv5M/0D1mAVE6FINC6MUQMJATEsjHFjIgtNTFxpxBBjiC7cCFETExdsdIvRhRoTNVqIxhATYQdCIKJIG10BLU6n0+nYPzPvOy7mDT4KtO/RTsF4T/JmMfPm/u4797v3u9/LBYKCgoKCgoKCgoKCgoKCbjExw30OQNT4YtWKVX3m/GYQD0PYCGINoA6JrQBIqgaxLGqYwKDI44AdLRZXHwPOTMfNeAACYBm4683nNoF6BNJGgGsAdV6HOyDy5zo3fxwYmErJzWSQb3Qwn893ONf6DI3PgniUdEvqt6j+KV3VPJlECZLOAfxK1EfF4tAvMxnN5sLhwMjIxTOzcDMZ5AFEa7G2bSw/9TKp3aS7MwYiblxxO7xGe42eN0bKEyRISDYN4TOaf7tQPn828V81uL29vUsr5eouEq/cAFcJ/hVcCp/Cau8UyoWZ3NQGXQ7t7nzPVjnsJ9wD9c6p4bjLMEWTsvjKkQ6SKpT2FkpD78W/5wDUuvM9j4ncT/L+JnHHRe0rFi9z3bWmHK9jDgFYd77ndZHvkqCkWjyyXKD1T3Ek1Dts+gZ++rmRkZGxro7b3iTcPhBoOld2aLrG58fHL1661pTj9SKnK9/zAZ17STJLONwMCUCNdC0yOyrqrKN/YTG5kP1aE54olYb+nBlJMw3KAah15Xs+dM69aLJq/B3RfEUkfbzoRvFoLoaqJFskDdLXthQKhfNJk9xMczpX9rzBf81pWSRzAMBLskU2BwBaJNVI3mOR/3LdunWtyYXfJ7NVR8eqLd75T+J5n1tEc2ZO8cWWA1B19Hf9XZnsmJyq9Df2SpfTZG9vb2tlvHrSkX03YRRvFdVI5iJxy+johZ8AeBcbYZVydZej60tkjawptBZnAEtkilpiz9IMLTSX9Y/o/Tiq6hHU2dm5kmo5B3BV3KjL0EGQdI0d6+X8wCt2sY3NnV9AY5rFjUh6g54sFof6cwAEa9lJ51dLlqWxZNY5Rul7gSeNGCajGuA6YLaBwFaQO0i2LtDUTXJPgPadIp6Q4xAZVQHXSbP1ALeB2EGyTZJlXNsEw24A/QSArnzPETpuihvyaTsp6TcD9oyODn07281dXbdvlGmfo3s64yBcjztAaU+hNPz1bNOoe8WaDfL2FomdGQdHAKqRtNF3L+++F97tjbNWGpet3kk7HGlqR6lUOBXHdC6RhRqXB8CJifHhycnK522ty5xz3BaHfdZsVeeajlTNPz46dvHEnNzp8qWJyfEvlra1G+m2Z+BGpGshMOjb21fuJPlUyj8bSZr0u/PR9mKxOBZ30BKLZPKyxJrmJ6cqh9va2u92dA9mNMkIUtAfESa3jo39VYz3aFEa7sRk5celbcvuIN1DcU2X4jnhJEw7CZuzrvSUdhUKhXJjc5mhUHSmqVclG25kidQhT9CA3aVSaTTmVrNw6WuvmexC4l3QXPsiAOpzAO6Lb2eK6HEmOzZSGv4hbqSWMfO4+gPyY9JxrncxM6bW6dHRof4b5RYKhTKkA6RDCi7rCZAdDlSv6g5xbhABsX8eRaTqEYiD8SC6dA9IADqYcQtyFVf0h1JyGd+3zAHMp3x5xnqk2+l5VtAy5wbMNJV+mglyODXPfZO8nx40s4m0XBLLXYbUF2/BXGG2N3ApDILZkjES4xnqJCDSfLgNlUmOZanRsoQrIcCcTc+7bPdlA2TIBKfNl5vL5UxqTNksoxMUDAoGBYOCQcGg/6AsGDRHVR8Mml0twaDZi+SwBs1eFgWD5vQoGBTSfDAoGBQMCgYFg4JBwaCgYNCNqH78JXVt0ox6Z8521QR2am6Wc4hsQvQS6U773ywu0x4CaDhKgAs0mpyK2Zqjs/XDT1yYKCKQlvt/PacZFBQUFBQUFBQUFBQUdKvrHxqD4m8WAQz0AAAAAElFTkSuQmCC';

/**
 * The style that draws the design's Meko markers (`MEKO_SOURCE_MARK`, after a key design decision's
 * title) as Meko's "m" in a round badge, for a page that has one; empty otherwise. Its text stays for
 * screen readers. It goes last, and `!important`, so the page's own styles don't undo it; the mark
 * takes the text color, so it follows the page's theme.
 */
export function mekoSourceStyle(html: string): string {
  if (!html.includes(MEKO_SOURCE_CLASS)) return '';
  const mask = `url("${MEKO_MARK_DATA_URL}") center / contain no-repeat`;
  return `<style>span.${MEKO_SOURCE_CLASS} { display: inline-block !important; box-sizing: content-box !important; width: 11px !important; height: 11px !important; margin: 0 0.3em !important; padding: 3px 6px !important; border-radius: 999px !important; font-size: 0 !important; vertical-align: middle !important; background: color-mix(in srgb, currentColor 14%, transparent) !important; position: relative !important; }
span.${MEKO_SOURCE_CLASS}::before { content: "" !important; position: absolute !important; inset: 3px 6px !important; background: currentColor !important; -webkit-mask: ${mask} !important; mask: ${mask} !important; }</style>`;
}

/**
 * The frame's srcdoc: the policy as the very first element, the app's theme applied, Meko's mark
 * drawn where the design names a decision from Meko, and Mermaid loaded when the page relies on the
 * viewer for its diagrams.
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
  return `<!doctype html>${open}${meta}${html.replace(/^\uFEFF?\s*<!doctype[^>]*>/i, '')}${mekoSourceStyle(html)}${mermaidLoader(html)}`;
}
