import { closeSync, fstatSync, openSync, readSync, realpathSync } from 'node:fs';
import { dirname, relative, sep } from 'node:path';

/**
 * Claude Code's notice for a tool result too large for Claude's context, which it saved to a file
 * instead. Only the notice reaches Claude and the transcript.
 */
export const TOO_LARGE = /^Error: result \(([\d,]+) characters\) exceeds maximum allowed tokens/;

/** The same notice, naming the file the result was saved to. */
const SAVED_REPLY = /^Error: result \([\d,]+ characters\) exceeds maximum allowed tokens\. Output has been saved to (\/\S+?)\.?(?:\s|$)/;

/**
 * The file a too-large notice names, when it's in a `tool-results` folder of a session beside the
 * transcript (`<transcripts>/<claude session>/tool-results/`, checked after `realpath`); null for any
 * other text, any other file, or one that doesn't exist.
 */
export function savedReplyFile(text: string | null, transcriptPath: string | null): string | null {
  const saved = text === null ? null : SAVED_REPLY.exec(text)?.[1];
  if (!saved || !transcriptPath) return null;
  try {
    const file = realpathSync(saved);
    const parts = relative(realpathSync(dirname(transcriptPath)), file).split(sep);
    return parts.length === 3 && parts[0] !== '..' && parts[1] === 'tool-results' ? file : null;
  } catch {
    return null;
  }
}

/** A saved reply's text, read at once; null when there's none, it can't be read, or it's over `maxBytes`. */
export function readSavedReplySync(text: string, transcriptPath: string, maxBytes: number): string | null {
  const file = savedReplyFile(text, transcriptPath);
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
