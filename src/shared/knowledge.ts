import { KB_UPLOAD_EXTS } from './types.js';

/** Whether Meko's knowledge base takes a file by this name: its extension, in any case, is in KB_UPLOAD_EXTS. */
export function isKnowledgeFile(name: string): boolean {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) return false;
  return (KB_UPLOAD_EXTS as readonly string[]).includes(name.slice(dot + 1).toLowerCase());
}

/** The types the knowledge base takes, in words: "PDF, TXT, MD, …". */
export const KB_TYPES_TEXT = 'PDF, TXT, Markdown, JSON, CSV, XML or HTML';

/** The file picker's `accept` for knowledge-base files. */
export const KB_ACCEPT = KB_UPLOAD_EXTS.map((e) => `.${e}`).join(',');
