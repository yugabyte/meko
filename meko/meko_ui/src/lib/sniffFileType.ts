/**
 * Content sniffing for knowledge-base uploads — the client-side counterpart to
 * api_server's resolveUploadMIME / distinctiveMIME / mimeIsTextual.
 *
 * The server used to sniff every upload, because every upload passed through
 * it. Presigned uploads go straight to the bucket, so nothing on our side sees
 * the bytes: a video renamed report.pdf would be stored and queued, then fail
 * somewhere in the indexing pipeline with nothing useful to show the user.
 * This catches it before the transfer starts.
 *
 * It is **not** a security control — it runs on the client, so a hostile
 * caller skips it. It buys an immediate, honest error. Enforcement lives on
 * the server, which re-sniffs the stored object over a ranged GET.
 *
 * Detection is delegated to `magic-bytes.js`, which walks a trie of magic
 * numbers and contains no format parsers. That is why it was chosen over the
 * more popular `file-type`: every advisory ever filed against that package has
 * been a hang or a blowup inside a container parser it reaches only on
 * malformed input — a ZIP decompression bomb, infinite loops in the ASF and
 * MKV readers. Screening files that are not what they claim to be is this
 * function's entire job, so those parsers were liability without upside. A
 * trie walk over a fixed-length prefix terminates by construction.
 *
 * The trade is that only formats with a signature are recognised, which of our
 * nine accepted extensions means PDF reliably and XML only when the optional
 * declaration is present. (magic-bytes.js's own "JSON" entry is the single
 * byte `{` or `[` — not a signature by this file's standard, so it is
 * filtered out of every result; see the comment in verifyFileContent.)
 * Everything else has nothing for the trie to match, so it is corroborated
 * the only way it can be: the bytes must not be a recognised *binary*
 * format, must decode as text, and — for JSON/XML/HTML, which have no real
 * magic number but do have a recognisable shape — must be shaped like one:
 * JSON for real, with JSON.parse; XML/HTML with a truncation-safe prefix
 * check (see verifyStructuredText).
 *
 * Every type named below is one the pipeline can index
 * (SUPPORTED_DOCUMENT_TYPES in yugabyte-db/python/ai/rag_agent/rag_pipeline/
 * document_types.py); the accepted set itself comes from
 * KNOWLEDGE_UPLOAD_MIME_BY_EXT so there is one list to keep true, and anything
 * outside it is rejected here rather than deferred.
 */
import {
  KNOWLEDGE_UPLOAD_MIME_BY_EXT,
  knowledgeFileExtension,
  unsupportedKnowledgeFileMessage,
} from "./knowledgeFileUploadRules";
import { FILE_CHANGED_WHILE_READING, isFileSnapshotError } from "./sha256File";

/** Bytes inspected. Matches mimetype's default limit server-side. */
const SNIFF_BYTES = 3072;

/**
 * MIME types that carry text. Detecting one of these never rejects a file: our
 * text extensions are not distinguishable from each other by content — JSON is
 * also valid Markdown as far as a sniffer is concerned, and a leading BOM
 * makes any of them look like text/plain — so only a *binary* verdict says
 * anything worth acting on.
 */
const TEXTUAL_MIMES = new Set([
  "text/plain",
  "text/markdown",
  "text/csv",
  "text/html",
  "text/xml",
  "application/xml",
  "application/json",
]);

/**
 * Types that always carry a signature, making its absence evidence of a
 * mismatch. PDF alone: the spec puts %PDF- at byte 0. XML is deliberately not
 * here, because the declaration is optional and `<root/>` is valid XML with
 * nothing to match — a binary file renamed .xml is still caught, either as a
 * recognised binary format or by failing the text and structural checks below.
 */
const SIGNATURE_BEARING = new Set(["application/pdf"]);

/**
 * The WHATWG MIME Sniffing spec's HTML signature list
 * (https://mimesniff.spec.whatwg.org/#matching-a-mime-type-pattern, "text/html"
 * rows) — the same tag-prefix check every browser runs on an untrusted
 * response body. Copied inline rather than taking a dependency on it: the one
 * npm port (`detect-content-type`) has been unmaintained since 2022, and the
 * list itself is short, stable spec text.
 */
const HTML_SIGNATURE_TAGS = [
  "<!DOCTYPE HTML",
  "<HTML",
  "<HEAD",
  "<SCRIPT",
  "<IFRAME",
  "<H1",
  "<DIV",
  "<FONT",
  "<TABLE",
  "<A",
  "<STYLE",
  "<TITLE",
  "<B",
  "<BODY",
  "<BR",
  "<P",
  "<!--",
];

/** Case-insensitive match of `text` against the spec's HTML tag list, each
 *  one requiring the byte right after it to be a space or '>' (the spec's own
 *  boundary rule) so e.g. `<B` doesn't match `<Bogus`. */
function matchesHtmlSignature(text: string): boolean {
  const upper = text.toUpperCase();
  return HTML_SIGNATURE_TAGS.some((tag) => {
    if (!upper.startsWith(tag)) return false;
    const next = text[tag.length];
    return next === " " || next === ">";
  });
}

/**
 * A shallow, truncation-safe structural check for the expected types
 * magic-bytes can't corroborate at all (no fixed magic number). Used as the
 * fallback in verifyStructuredText when `head` is only a partial read of the
 * file — a document actually shaped like JSON/markup still starts that way;
 * ordinary prose or a CSV essentially never does.
 *
 * text/xml stays a bare `<` check: this branch only runs for XML *without* a
 * `<?xml` declaration (declared XML is already caught earlier, via
 * magic-bytes' own signature), and the WHATWG spec has no lighter-weight
 * signature for undeclared XML either — `<?xml` is its only XML entry too.
 */
const STRUCTURAL_PREFIX_CHECK: Record<string, (text: string) => boolean> = {
  "application/json": (text) => /^[[{]/.test(text),
  "text/xml": (text) => text.startsWith("<"),
  "text/html": matchesHtmlSignature,
};

/** Strips a leading UTF-8 BOM and whitespace so the checks below look at the
 *  first real character, not decoration ahead of it. */
function stripLeadingDecoration(text: string): string {
  return text.replace(/^﻿/, "").trimStart();
}

/**
 * Verifies JSON with the real thing — JSON.parse, the engine that will
 * actually consume the file later — instead of a hand-rolled heuristic. Never
 * reads more than the `head` it's given (never more than SNIFF_BYTES, and
 * never re-reads the file to get more).
 *
 * Whether to trust a JSON.parse failure turns on `wholeFile`: a truncated
 * `head` naturally fails to parse too (it hasn't reached its closing bracket
 * yet), which isn't evidence of anything. An earlier version tried
 * distinguishing "ran out of input" from "hit a bad token" by matching
 * JSON.parse's error *message* — mirroring how gabriel-vasile/mimetype
 * tolerates `io.ErrUnexpectedEOF` server-side — but V8 alone has five-plus
 * message shapes for truncation and the wording isn't a stable contract
 * across engine versions. `wholeFile` sidesteps that entirely: when `head`
 * already contains every byte the file has, JSON.parse's verdict is exact.
 *
 * XML/HTML have no equivalent engine to defer to (DOMParser's HTML mode has
 * no error state at all — the HTML5 spec defines it to always produce *some*
 * tree — and XML hits the same message-parsing problem), so they stay on the
 * plain prefix check regardless of `wholeFile`.
 */
function verifyStructuredText(expected: string, head: Uint8Array, wholeFile: boolean): boolean {
  const text = stripLeadingDecoration(new TextDecoder("utf-8", { fatal: false }).decode(head));

  if (expected === "application/json") {
    try {
      JSON.parse(text);
      return true;
    } catch {
      if (wholeFile) return false;
      // No more of the file to consult -- fall through to the same
      // truncation-safe shape check XML/HTML always use.
    }
  }

  const prefixCheck = STRUCTURAL_PREFIX_CHECK[expected];
  return prefixCheck ? prefixCheck(text) : true;
}

/**
 * Verifies a file's type and content. Returns a message to show the user, or
 * null when the file looks acceptable.
 *
 * Within the accepted types this is biased toward accepting, because a false
 * rejection blocks a legitimate upload with no workaround while a false
 * acceptance only defers the error to the server.
 *
 * An unsupported extension is refused on the name alone, before a single byte
 * is read: no sniffing can rescue a type the pipeline has no way to index, so
 * there is nothing to weigh. Callers should reject it earlier still (see
 * unsupportedKnowledgeFileMessage) — both run before the hash and the presign,
 * so a rejected file never pays for either.
 */
export async function verifyFileContent(file: File): Promise<string | null> {
  const unsupported = unsupportedKnowledgeFileMessage(file.name);
  if (unsupported) return unsupported;

  const expected = KNOWLEDGE_UPLOAD_MIME_BY_EXT[knowledgeFileExtension(file.name)]!;

  // One read serves both checks below.
  let head: Uint8Array;
  try {
    head = new Uint8Array(await file.slice(0, SNIFF_BYTES).arrayBuffer());
  } catch (err) {
    if (isFileSnapshotError(err)) return FILE_CHANGED_WHILE_READING;
    throw err;
  }
  // A read of an unexpected length means the file shrank since it was selected;
  // `size` is the snapshot value, so it and the bytes should agree. Growth is
  // caught downstream by the hash, which runs after this and reads the whole
  // file.
  if (head.length !== Math.min(SNIFF_BYTES, file.size)) {
    return FILE_CHANGED_WHILE_READING;
  }

  const rawDetected = await detectMimes(head);
  // null means the detector itself failed to load (offline, a stale chunk
  // 404ing after a deploy, CSP) -- distinct from [], which means it loaded
  // and walked the trie but found no signature. Only the latter can turn
  // into a rejection below; the former has nothing to check and must not
  // block the upload on that account.
  if (rawDetected === null) return null;
  // magic-bytes.js reports "application/json" for any bytes starting with
  // `{` or `[`, with nothing behind it -- unlike its other entries, that is
  // not a signature, just the same shallow prefix rule STRUCTURAL_PREFIX_CHECK
  // uses as its own fallback. Trusting it here would let `compatible` below
  // accept "{not valid json" outright, short-circuiting past the real
  // JSON.parse in verifyStructuredText that is meant to be JSON's actual
  // authority.
  const detected = rawDetected.filter((mime) => mime !== "application/json");

  if (detected.some((mime) => compatible(mime, expected))) return null;

  // A positively recognised binary format is the strongest signal available,
  // and the only one specific enough to name the file's real type back.
  const conflicting = detected.find((mime) => !TEXTUAL_MIMES.has(mime));
  if (conflicting) {
    return (
      `This ${knowledgeFileExtension(file.name)} file doesn't look like ${label(expected)} ` +
      `inside — it looks like ${label(conflicting)}. ` +
      `Rename it with the matching extension, or upload the original file.`
    );
  }

  // Nothing binary matched. Where a signature is mandatory, that absence is
  // itself the answer — prose renamed report.pdf is text, but is not a PDF.
  if (SIGNATURE_BEARING.has(expected)) {
    return (
      `This file doesn't look like ${label(expected)} inside. ` +
      `Check that it wasn't renamed, and that it isn't damaged.`
    );
  }

  // A text format, where "no signature" is the expected answer. All that is
  // left to confirm is that it really is text.
  if (!isTextual(head)) {
    return "This file isn't readable as text, so there's nothing to index. Upload the original file instead.";
  }

  // Some text formats have a recognisable shape even without a byte
  // signature — see verifyStructuredText.
  const wholeFile = head.length === file.size;
  if (expected in STRUCTURAL_PREFIX_CHECK && !verifyStructuredText(expected, head, wholeFile)) {
    return (
      `This file doesn't look like ${label(expected)} inside. ` +
      `Check that it wasn't renamed, and that it isn't damaged.`
    );
  }

  return null;
}

/**
 * Every MIME type the head's magic number could belong to, normalised and
 * lowercased. Empty when the detector loaded and found nothing, which is the
 * normal answer for text; null when the detector itself failed to load (see
 * the caller in verifyFileContent) -- the two are not the same "no match".
 *
 * More than one candidate is routine rather than an error: a zip prefix alone
 * matches .docx, .apk and nine others, so the trie reports all of them and the
 * caller accepts the file if *any* candidate fits.
 *
 * Loaded on demand — the signature trie is ~6 KB gzipped, which no one should
 * pay for on first paint when it is only reachable from picking a file to
 * upload. Sniffing is advisory, so a failure to load the chunk at all (offline,
 * a stale chunk 404ing after a deploy, CSP) must not block the upload either —
 * that is what the null return communicates to the caller.
 */
async function detectMimes(head: Uint8Array): Promise<string[] | null> {
  try {
    const { filetypemime } = await import("magic-bytes.js");
    // Some entries carry parameters, e.g. "text/plain; charset=UTF-8".
    return filetypemime(head).map((mime) => mime.split(";")[0]!.trim().toLowerCase());
  } catch {
    return null;
  }
}

/**
 * Whether a detected type is an acceptable stand-in for the one the extension
 * promised. Exact matches aside, this is where text-vs-text disagreement is
 * forgiven; see TEXTUAL_MIMES.
 */
function compatible(detected: string, expected: string): boolean {
  if (detected === expected) return true;
  return TEXTUAL_MIMES.has(detected) && TEXTUAL_MIMES.has(expected);
}

/**
 * True when the head decodes as UTF-8 and holds no NUL bytes — the practical
 * version of the server's "sits under text/plain" walk up mimetype's
 * hierarchy.
 */
function isTextual(head: Uint8Array): boolean {
  if (head.length === 0) return true; // Empty file; the size checks own this.
  if (head.includes(0)) return false;
  try {
    // `stream: true` tolerates a multi-byte character cut in half by the 3 KB
    // boundary, which would otherwise read as invalid UTF-8 and mislabel a
    // perfectly good text file as binary.
    new TextDecoder("utf-8", { fatal: true }).decode(head, { stream: true });
    return true;
  } catch {
    return false;
  }
}

/**
 * Name for a MIME type as a non-technical uploader would say it, for use inside
 * a sentence. Kept in step with api_server's `uploadTypeName` so a file gets
 * the same words whether this check or the server's catches it.
 */
function label(mime: string): string {
  switch (mime) {
    case "application/pdf":
      return "a PDF";
    case "text/html":
      return "a web page";
    case "text/xml":
    case "application/xml":
      return "an XML file";
    case "application/json":
      return "a JSON file";
    case "text/csv":
      return "a CSV spreadsheet";
    case "text/markdown":
      return "Markdown";
    case "text/plain":
      return "plain text";
    case "image/png":
    case "image/jpeg":
    case "image/gif":
    case "image/webp":
      return "an image";
    default:
      return mime;
  }
}
