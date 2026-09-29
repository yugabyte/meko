import { readFileSync, statSync } from 'node:fs';
import { extname, resolve } from 'node:path';

/** A published Artifact's HTML larger than this isn't kept (the claude.ai link still is). */
export const MAX_DESIGN_HTML = 5 * 1024 * 1024;
/** A markdown design file larger than this is ignored. */
export const MAX_DESIGN_MARKDOWN = 1024 * 1024;

const ARTIFACT_URL = /https:\/\/claude\.ai\/(?:code\/)?artifact\/[A-Za-z0-9_-]+/;

/** The first claude.ai Artifact link in some text (a tool result, a design file), if any. */
export function artifactUrlIn(text: string): string | null {
  return ARTIFACT_URL.exec(text)?.[0] ?? null;
}

/**
 * The Meko filename for version `version` (from 1) of a session's design. Meko can't replace a
 * file, so each later version gets its own name: design-<slug>.md, design-<slug>-v2.md, …
 */
export function mekoDesignName(slug: string, ext: 'md' | 'html', version: number): string {
  return `design-${slug}${version > 1 ? `-v${version}` : ''}.${ext}`;
}

/** True for an Artifact tool call that publishes a page (not a read, list, asset upload…). */
export function isArtifactPublish(input: Record<string, unknown>): boolean {
  const action = input.action;
  return (action === undefined || action === 'publish') && !input.asset && typeof input.file_path === 'string';
}

/** The HTML page an Artifact was published from, or null when it isn't a readable .html file. */
export function readArtifactHtml(filePath: unknown, cwd: string | undefined): string | null {
  if (typeof filePath !== 'string' || !/^\.html?$/i.test(extname(filePath))) return null;
  return readCapped(resolve(cwd || '/', filePath), MAX_DESIGN_HTML);
}

/** A markdown design file's text, or null when it's missing, empty or too large. */
export function readDesignFile(file: string): string | null {
  const text = readCapped(file, MAX_DESIGN_MARKDOWN);
  return text?.trim() ? text : null;
}

function readCapped(file: string, max: number): string | null {
  try {
    const st = statSync(file);
    if (!st.isFile() || st.size > max) return null;
    return readFileSync(file, 'utf8');
  } catch {
    return null;
  }
}
