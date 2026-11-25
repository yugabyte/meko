import { closeSync, fstatSync, openSync, readSync, realpathSync } from 'node:fs';
import { basename, dirname, relative, sep } from 'node:path';

/**
 * Claude Code's notice for a tool result too large for Claude's context, naming the file it saved
 * the result to. Only the notice reaches Claude and the transcript.
 */
const SAVED_REPLY = /^Error: result \([\d,]+ characters\) exceeds maximum allowed tokens\. Output has been saved to (\/\S+?)\.?(?:\s|$)/;

/** The largest saved reply read: a bigger one is left unread. */
export const SAVED_REPLY_MAX_BYTES = 8 * 1024 * 1024;

/**
 * The file a too-large notice names, when it's in the session's own `tool-results` folder
 * (`<transcripts>/<claude session>/tool-results/` for the main transcript
 * `<transcripts>/<claude session>.jsonl`, which its subagents' replies go to as well), checked after
 * `realpath`; null for any other text, any other file (another session's included), or one that
 * doesn't exist.
 */
export function savedReplyFile(text: string, mainTranscript: string): string | null {
  const saved = SAVED_REPLY.exec(text)?.[1];
  if (!saved) return null;
  try {
    const file = realpathSync(saved);
    const parts = relative(realpathSync(dirname(mainTranscript)), file).split(sep);
    const own = basename(mainTranscript, '.jsonl');
    return parts.length === 3 && parts[0] === own && parts[1] === 'tool-results' ? file : null;
  } catch {
    return null;
  }
}

/**
 * The reply behind a too-large notice, read at once; null when there's none, it can't be read, or
 * it's over `maxBytes`. Used only for Meko's figure of what a search saved (`mekoResultOf`).
 */
export function readSavedReplySync(text: string, mainTranscript: string, maxBytes = SAVED_REPLY_MAX_BYTES): string | null {
  const file = savedReplyFile(text, mainTranscript);
  if (!file) return null;
  let fd: number | undefined;
  try {
    fd = openSync(file, 'r');
    const size = fstatSync(fd).size;
    if (size > maxBytes) return null;
    const buf = Buffer.alloc(size);
    let got = 0;
    while (got < size) {
      const n = readSync(fd, buf, got, size - got, got);
      if (n <= 0) break;
      got += n;
    }
    return buf.subarray(0, got).toString('utf8');
  } catch {
    return null;
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}
