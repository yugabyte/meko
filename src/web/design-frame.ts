// A published Artifact's HTML, prepared for the design panel's sandboxed frame.
//
// The frame is sandboxed without allow-same-origin, so the page runs in an opaque origin:
// it can't read AgentK Local's cookies or call its API. The policy below also limits what it
// can load to what Artifacts themselves may use (inline code, the two script CDNs, Google
// Fonts, data: and blob: media), with no network requests of its own.

export const DESIGN_FRAME_CSP = [
  "default-src 'none'",
  "script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net",
  "style-src 'unsafe-inline' https://fonts.googleapis.com",
  'font-src https://fonts.gstatic.com data:',
  'img-src data: blob:',
  'media-src data: blob:',
].join('; ');

/**
 * The frame's srcdoc: the policy as the very first element, and the app's theme applied.
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
  return `<!doctype html>${open}${meta}${html.replace(/^\uFEFF?\s*<!doctype[^>]*>/i, '')}`;
}
