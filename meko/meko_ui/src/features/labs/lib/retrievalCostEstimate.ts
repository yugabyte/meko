// Estimates the input-token cost of the two ways to get a standards
// document into a model, for the "how it reads the standards" lesson.
//
// These are ESTIMATES and every caller must label them as such. The real,
// measured numbers come from a review's own `usage.input_tokens` and
// `context_stats` (see deploymentInvokeApi.ts) and only exist after a run.

/** Characters per token. The usual rough figure for English prose with
 *  Anthropic/Bedrock tokenizers; neither exposes a pre-call tokenizer in the
 *  browser, which is why this is an approximation rather than a count. */
const CHARS_PER_TOKEN = 4;

/** What a retrieval actually pulls back for one diff: the handful of rules
 *  that match, not the document. Matches the shape review.py's
 *  knowledgebase_search returns for the sample PR. */
const TYPICAL_CHUNKS_RETRIEVED = 2;

/** Bytes per token, the same rough figure as CHARS_PER_TOKEN: a knowledge
 *  document is text, so one byte is about one character. */
const BYTES_PER_TOKEN = 4;

export function estimateTokens(text: string): number {
  return Math.round(text.length / CHARS_PER_TOKEN);
}

/** Input cost of pasting the user's OWN uploaded documents into every prompt.
 *
 *  Prefers the CHUNK count over the file size, because chunks are counted
 *  after text extraction and file size isn't: a 5 KB .txt and a 5 KB .md hold
 *  roughly the same amount of text, but a 5 KB PDF is mostly structure and
 *  fonts, and a large PDF can carry less text than a small Markdown file.
 *  Counting chunks sidesteps the format entirely.
 *
 *  Falls back to bytes only when the index hasn't reported a chunk size, and
 *  returns null when there is nothing uploaded at all, so callers show the
 *  bundled sample rather than a confident zero.
 *
 *  Both paths are estimates and every caller labels them as such. The chunk
 *  path also runs slightly high, since chunks overlap and the same characters
 *  are counted twice at each boundary. */
export function estimatePasteTokensFromDocuments(
  documents: { chunks_processed?: number | null; size_bytes?: number | null }[],
  chunkSizeInChars: number | null,
): number | null {
  const totalChunks = documents.reduce<number>((sum, doc) => sum + (doc.chunks_processed ?? 0), 0);
  if (chunkSizeInChars && chunkSizeInChars > 0 && totalChunks > 0) {
    return Math.round((totalChunks * chunkSizeInChars) / CHARS_PER_TOKEN);
  }
  const totalBytes = documents.reduce<number>((sum, doc) => sum + (doc.size_bytes ?? 0), 0);
  if (totalBytes <= 0) return null;
  return Math.round(totalBytes / BYTES_PER_TOKEN);
}

/** Cost of pasting the whole document into every prompt. */
export function estimatePasteTokens(documentContent: string): number {
  return estimateTokens(documentContent);
}

/** Cost of retrieving only the rules that apply to one diff — the document's
 *  per-chunk average times the number a search typically returns. */
export function estimateQueryTokens(documentContent: string, totalChunks: number): number {
  if (totalChunks <= 0) return 0;
  const perChunk = estimateTokens(documentContent) / totalChunks;
  return Math.round(perChunk * Math.min(TYPICAL_CHUNKS_RETRIEVED, totalChunks));
}

/** How many times more input the paste approach costs. Returns null when
 *  there's nothing meaningful to compare, so callers can hide the claim
 *  rather than print "0×" or divide by zero. */
export function costMultiple(pasteTokens: number, queryTokens: number): number | null {
  if (queryTokens <= 0 || pasteTokens <= 0) return null;
  return Math.round(pasteTokens / queryTokens);
}
