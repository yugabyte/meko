import { KB_TYPES_TEXT, isKnowledgeFile } from '../shared/knowledge';
import type { ServerEvent } from '../shared/types';

/**
 * Files dropped on a terminal, sorted: `images` (as `pastedImages` found them) are held for Claude,
 * `knowledge` files can go to the knowledge base, and `other` can go nowhere.
 */
export function sortDrop<F extends { name: string }>(files: F[], images: F[]): { images: F[]; knowledge: F[]; other: F[] } {
  const rest = files.filter((f) => !images.includes(f));
  return { images, knowledge: rest.filter((f) => isKnowledgeFile(f.name)), other: rest.filter((f) => !isKnowledgeFile(f.name)) };
}

/** The notice for dropped files that can go nowhere: what was skipped, and what can be dropped. */
export function skippedNotice(other: { name: string }[]): string {
  const what = other.length === 1 ? other[0].name : `${other.length} files`;
  return `Skipped ${what}. Drop images for Claude, or ${KB_TYPES_TEXT} files for the knowledge base.`;
}

/** What the terminal says when a file added to the knowledge base is done indexing (a `knowledge` event). */
export function knowledgeNotice(ev: Extract<ServerEvent, { type: 'knowledge' }>): string {
  if (ev.result === 'indexed') return `${ev.filename} is in the knowledge base.`;
  if (ev.result === 'failed') return `Meko couldn’t index ${ev.filename}.`;
  return `Meko is still indexing ${ev.filename}. Search the knowledge base later to check it.`;
}

/** Human-readable size: "820 B", "14 KB", "3.2 MB". */
export function fileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

/** Opens the browser's file picker (from a click or tap) and resolves with what was picked; nothing on cancel. */
export function pickFiles(accept: string): Promise<File[]> {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = accept;
    input.multiple = true;
    input.addEventListener('change', () => resolve(Array.from(input.files ?? [])));
    input.addEventListener('cancel', () => resolve([]));
    input.click();
  });
}
