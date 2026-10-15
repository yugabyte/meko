import { KB_TYPES_TEXT, isKnowledgeFile } from '../shared/knowledge';
import { MAX_KB_UPLOAD_BYTES, type KnowledgeUpload, type ServerEvent } from '../shared/types';

/**
 * Where one file for the knowledge base is: not sent yet, sending, in Meko (`indexing`: AgentK
 * reports when it's done; `added`: Meko took it with nothing to follow; `already`: Meko had it), or refused.
 */
export type KnowledgeFileState =
  | { at: 'waiting' }
  | { at: 'sending' }
  | { at: 'indexing' }
  | { at: 'added' }
  | { at: 'already' }
  | { at: 'failed'; error: string };

/**
 * Files waiting on a session's terminal to be added to the knowledge base: the ones that can go,
 * each with where it is, and the ones skipped (empty, or over the limit). Kept on the terminal, which
 * outlasts its pane, so a dialog opened again shows the uploads still running.
 */
export interface KnowledgeBatch<F extends { name: string; size: number } = File> {
  files: F[];
  states: KnowledgeFileState[];
  skipped: F[];
  /** An upload is running: the batch can't be closed or sent again. */
  busy: boolean;
}

/** A batch for these files, none sent yet. */
export function knowledgeBatch<F extends { name: string; size: number }>(files: F[]): KnowledgeBatch<F> {
  const fits = files.filter((f) => f.size > 0 && f.size <= MAX_KB_UPLOAD_BYTES);
  return { files: fits, states: fits.map(() => ({ at: 'waiting' })), skipped: files.filter((f) => !fits.includes(f)), busy: false };
}

/** Where a file is once the server answered its upload. */
export function uploadedState(r: KnowledgeUpload): KnowledgeFileState {
  return { at: r.status === 'processing' ? 'indexing' : r.status === 'uploaded' ? 'added' : 'already' };
}

/** The terminal notice once a batch's uploads are over: what was added, what was there, what failed. Null when none went. */
export function batchSummary(b: KnowledgeBatch<{ name: string; size: number }>): string | null {
  const count = (at: KnowledgeFileState['at']) => b.states.filter((s) => s.at === at).length;
  const nFiles = (n: number) => (n === 1 ? '1 file' : `${n} files`);
  const indexing = count('indexing');
  const added = indexing + count('added');
  const already = count('already');
  const failed = count('failed');
  const parts: string[] = [];
  if (added) {
    const what = added === 1 ? b.files[b.states.findIndex((s) => s.at === 'indexing' || s.at === 'added')].name : nFiles(added);
    const them = indexing === added ? (added === 1 ? 'it' : 'them') : `${indexing} of them`;
    parts.push(`${what} added to the knowledge base.${indexing ? ` Meko is indexing ${them}.` : ''}`);
  }
  if (already) parts.push(`${nFiles(already)} ${already === 1 ? 'was' : 'were'} already there.`);
  if (failed) parts.push(`${nFiles(failed)} couldn’t be added.`);
  return parts.length ? parts.join(' ') : null;
}

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
  if (ev.result === 'failed') return `Meko couldn’t index ${ev.filename}. Check that it opens and holds text, then drop it again.`;
  return `AgentK couldn’t tell whether Meko indexed ${ev.filename}. Search the knowledge base for it later.`;
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
