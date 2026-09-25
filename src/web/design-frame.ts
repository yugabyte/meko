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

/** The frame's srcdoc: the page with the policy first in <head> and the app's theme applied. */
export function designFrameDoc(html: string, theme: string | null): string {
  const meta = `<meta http-equiv="Content-Security-Policy" content="${DESIGN_FRAME_CSP}">`;
  // A policy in the page itself can only narrow this one, never widen it.
  let doc = /<head[^>]*>/i.test(html) ? html.replace(/<head[^>]*>/i, (m) => m + meta) : meta + html;
  // Artifacts theme themselves from prefers-color-scheme, overridden by data-theme on <html>.
  if (theme === 'light' || theme === 'dark') {
    doc = /<html[\s>]/i.test(doc) ? doc.replace(/<html(?=[\s>])/i, `<html data-theme="${theme}"`) : doc;
  }
  return doc;
}
