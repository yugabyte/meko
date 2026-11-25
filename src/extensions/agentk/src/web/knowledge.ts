import { KB_TYPES_TEXT, isKnowledgeFile } from '../shared/knowledge';
import type { KnowledgeUpload, ServerEvent } from '../shared/types';

/** The image types the host takes as images (checked by their bytes there): anything else held is a file. */
const PASTABLE_IMAGES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];

/** Whether a held blob goes up as an image (Claude Code attaches it) rather than as a file. */
export function isPastableImage(b: { type: string }): boolean {
  return PASTABLE_IMAGES.includes(b.type);
}

/** A held file's extension for its tile ("PDF", "MD"), or "FILE" when its name has none. */
export function fileBadge(name: string): string {
  const dot = name.lastIndexOf('.');
  const ext = dot > 0 ? name.slice(dot + 1) : '';
  return ext && ext.length <= 5 ? ext.toUpperCase() : 'FILE';
}

/** Which held files Upload to Meko takes (`upload`: a type the knowledge base takes) and which it skips. */
export function mekoPlan<T extends { name: string }>(held: readonly T[]): { upload: T[]; skip: T[] } {
  return { upload: held.filter((h) => isKnowledgeFile(h.name)), skip: held.filter((h) => !isKnowledgeFile(h.name)) };
}

/** "notes.md", "notes.md and spec.pdf", "3 files". */
export function fileList(names: string[]): string {
  if (names.length === 1) return names[0];
  if (names.length === 2) return `${names[0]} and ${names[1]}`;
  return `${names.length} files`;
}

/** The question Upload to Meko asks before anything leaves: what goes, where, who can find it, and what it skips. */
export function mekoAsk(plan: { upload: { name: string }[]; skip: { name: string }[] }, datapack: string): string {
  const skip = plan.skip.length
    ? ` It skips ${fileList(plan.skip.map((s) => s.name || 'a pasted image'))}: the knowledge base takes ${KB_TYPES_TEXT} files.`
    : '';
  return `Upload ${fileList(plan.upload.map((u) => u.name))} to ${datapack}? Everyone with access to the datapack can find what's in ${plan.upload.length === 1 ? 'it' : 'them'}.${skip}`;
}

/** One file's Upload to Meko: the server's answer, or why it failed. */
export type MekoResult = { name: string; upload: KnowledgeUpload } | { name: string; error: string };

/** The terminal notice once Upload to Meko is over: what was added, what Meko already had, what failed and why. */
export function mekoSummary(results: MekoResult[]): string {
  const ok = results.filter((r): r is { name: string; upload: KnowledgeUpload } => 'upload' in r);
  const failed = results.filter((r): r is { name: string; error: string } => 'error' in r);
  const added = ok.filter((r) => r.upload.status === 'processing' || r.upload.status === 'uploaded');
  const there = ok.filter((r) => !added.includes(r));
  const indexing = added.filter((r) => r.upload.status === 'processing').length;
  const parts: string[] = [];
  if (added.length) {
    const them = indexing === added.length ? (added.length === 1 ? 'it' : 'them') : `${indexing} of them`;
    parts.push(`${fileList(added.map((r) => r.name))} added to the knowledge base.${indexing ? ` Meko is indexing ${them}.` : ''}`);
  }
  if (there.length) parts.push(`${fileList(there.map((r) => r.name))} ${there.length === 1 ? 'was' : 'were'} already there.`);
  if (failed.length) parts.push(`Couldn’t add ${fileList(failed.map((r) => r.name))}: ${failed[0].error}`);
  return parts.join(' ');
}

/** What the terminal says when a file added to the knowledge base is done indexing (a `knowledge` event). */
export function knowledgeNotice(ev: Extract<ServerEvent, { type: 'knowledge' }>): string {
  if (ev.result === 'indexed') return `${ev.filename} is in the knowledge base.`;
  if (ev.result === 'failed') return `Meko couldn’t index ${ev.filename}. Check that it opens and holds text, then drop it again.`;
  return `Kode couldn’t tell whether Meko indexed ${ev.filename}. Search the knowledge base for it later.`;
}

/** Opens the browser's file picker (from a click or tap) and resolves with what was picked; nothing on cancel. */
export function pickFiles(accept = ''): Promise<File[]> {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    if (accept) input.accept = accept;
    input.multiple = true;
    input.addEventListener('change', () => resolve(Array.from(input.files ?? [])));
    input.addEventListener('cancel', () => resolve([]));
    input.click();
  });
}
