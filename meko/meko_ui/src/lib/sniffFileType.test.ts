import { describe, expect, it, vi } from "vitest";

import { KNOWLEDGE_UPLOAD_MIME_BY_EXT } from "./knowledgeFileUploadRules";
import { verifyFileContent } from "./sniffFileType";

/** A minimal but genuinely detectable PDF: the %PDF- signature is the header. */
const PDF_BYTES = new TextEncoder().encode("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n");
/** A 1x1 PNG header, standing in for "some binary file that isn't a PDF". */
const PNG_BYTES = new Uint8Array([
  0x89,
  0x50,
  0x4e,
  0x47,
  0x0d,
  0x0a,
  0x1a,
  0x0a, // signature
  0x00,
  0x00,
  0x00,
  0x0d,
  0x49,
  0x48,
  0x44,
  0x52, // IHDR, length 13
  0x00,
  0x00,
  0x00,
  0x01,
  0x00,
  0x00,
  0x00,
  0x01, // 1x1
  0x08,
  0x06,
  0x00,
  0x00,
  0x00,
  0x00,
  0x00,
  0x00,
  0x00,
]);
/** A zip local-file header, which matches .docx/.apk/.jar/... all at once. */
const ZIP_BYTES = new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00, 0x08, 0x00]);
/** UTF-8 BOM. Enough on its own for the sniffer to report text/plain. */
const BOM = "\ufeff";

function file(name: string, body: Uint8Array | string): File {
  const bytes = typeof body === "string" ? new TextEncoder().encode(body) : body;
  return new File([bytes], name);
}

describe("verifyFileContent", () => {
  it("only accepts types the pipeline can index", () => {
    // Guards against drift from SUPPORTED_DOCUMENT_TYPES in
    // yugabyte-db/python/ai/rag_agent/rag_pipeline/document_types.py. A type
    // added here but not there uploads fine and then never gets indexed.
    expect(new Set(Object.values(KNOWLEDGE_UPLOAD_MIME_BY_EXT))).toEqual(
      new Set([
        "application/pdf",
        "application/json",
        "text/plain",
        "text/markdown",
        "text/csv",
        "text/xml",
        "text/html",
      ]),
    );
  });

  it("accepts a real PDF named .pdf", async () => {
    expect(await verifyFileContent(file("doc.pdf", PDF_BYTES))).toBeNull();
  });

  it("rejects a PNG renamed to .pdf, naming both types in plain words", async () => {
    const err = await verifyFileContent(file("doc.pdf", PNG_BYTES));
    expect(err).toContain("an image");
    expect(err).toContain("a PDF");
    // The person reading this uploaded a file; a MIME type means nothing to them.
    expect(err).not.toContain("image/png");
  });

  it("rejects a binary file renamed to a text extension", async () => {
    expect(await verifyFileContent(file("notes.md", PNG_BYTES))).toContain("an image");
  });

  it("rejects a container whose prefix matches many types at once", async () => {
    // The sniffer returns eleven candidates for a bare zip header; none is a
    // PDF, so the file is rejected rather than accepted on a partial match.
    expect(await verifyFileContent(file("doc.pdf", ZIP_BYTES))).toContain(
      "doesn't look like a PDF",
    );
  });

  it("rejects a .pdf that carries no PDF signature", async () => {
    // The reason this case can't fall through to the textual check: plain text
    // renamed report.pdf is text, but it is still not a PDF.
    expect(await verifyFileContent(file("doc.pdf", "just some prose"))).toContain(
      "doesn't look like a PDF",
    );
  });

  it("rejects a .pdf whose BOM makes it sniff as plain text", async () => {
    // text/plain is a detected type, so this must not short-circuit past the
    // signature check the way an unrecognised file would.
    expect(await verifyFileContent(file("doc.pdf", `${BOM}just some prose`))).toContain(
      "doesn't look like a PDF",
    );
  });

  it("accepts ordinary text for freeform text extensions", async () => {
    // .json and .html are deliberately excluded: unlike .txt/.md/.csv, they
    // have a recognisable shape (see below) and "hello, world" isn't it.
    for (const name of ["a.txt", "a.md", "a.csv"]) {
      expect(await verifyFileContent(file(name, "hello, world\n"))).toBeNull();
    }
  });

  it("accepts JSON and HTML shaped like JSON and HTML", async () => {
    expect(await verifyFileContent(file("a.json", '{"hello": "world"}'))).toBeNull();
    expect(await verifyFileContent(file("a.json", "[1, 2, 3]"))).toBeNull();
    expect(await verifyFileContent(file("a.html", "<!doctype html><p>hi</p>"))).toBeNull();
  });

  it("rejects prose renamed to .json or .html — it decodes as text but isn't shaped like either", async () => {
    // Closes the gap left by magic-bytes having no signature for JSON/HTML at
    // all: without the structural check, this passed client-side and was
    // only ever caught by the server's ResolveUploadMIME.
    expect(await verifyFileContent(file("notes.json", "hello, world\n"))).toContain(
      "doesn't look like a JSON file",
    );
    expect(await verifyFileContent(file("notes.html", "hello, world\n"))).toContain(
      "doesn't look like a web page",
    );
  });

  it("tolerates leading whitespace and a BOM ahead of JSON/markup", async () => {
    expect(await verifyFileContent(file("a.json", `${BOM}  \n  {"a": 1}`))).toBeNull();
    expect(await verifyFileContent(file("a.html", "  \n  <p>hi</p>"))).toBeNull();
  });

  it("rejects markup that starts with '<' but isn't shaped like HTML", async () => {
    // The WHATWG tag list is a real signature set, not "any '<'" -- neither
    // of these matches any entry in HTML_SIGNATURE_TAGS.
    expect(await verifyFileContent(file("weird.html", "<root>not html</root>"))).toContain(
      "doesn't look like a web page",
    );
    // <BOGUS> starts with the "<B" tag's letters but fails its boundary rule
    // (the character right after "<B" must be a space or '>', not 'O').
    expect(await verifyFileContent(file("weird2.html", "<BOGUS>not html</BOGUS>"))).toContain(
      "doesn't look like a web page",
    );
  });

  it("rejects malformed JSON that merely starts with the right character", async () => {
    // This is a real JSON.parse call, not just the opening-character check --
    // catching what the prefix check (still all XML/HTML gets, see below)
    // would let through.
    expect(await verifyFileContent(file("bad.json", "{not valid json"))).toContain(
      "doesn't look like a JSON file",
    );
  });

  it("does not catch mismatched XML nesting -- no engine to defer to the way JSON.parse lets JSON", async () => {
    // Documents a real limitation, not a target to close: unlike JSON.parse,
    // nothing here can tell a truncated well-formed document apart from a
    // genuinely malformed one for XML, so it stays on the plain prefix check
    // (see verifyStructuredText's doc comment). Still correctly caught
    // server-side.
    expect(await verifyFileContent(file("bad.xml", "<root><unclosed></root>"))).toBeNull();
  });

  it("accepts valid JSON/XML too large to fit in the sniff window", async () => {
    // Bigger than SNIFF_BYTES, so `head` is a truncated prefix: JSON.parse on
    // it throws (the document hasn't closed yet) even though the file in
    // full is perfectly valid JSON. That must read as inconclusive, not a
    // mismatch -- see verifyStructuredText's "unexpected end" handling.
    const jsonPad = `{"pad": "${"a".repeat(3100)}"}`;
    expect(await verifyFileContent(file("big.json", jsonPad))).toBeNull();
    const xmlPad = `<root>${"a".repeat(3100)}</root>`;
    expect(await verifyFileContent(file("big.xml", xmlPad))).toBeNull();
  });

  it("accepts text whose content sniffs as a different text format", async () => {
    // A .txt holding JSON, and a .md opening with a BOM, are both detected as
    // something other than what the extension says. Neither is worth blocking:
    // the text formats are indistinguishable by content and all index alike.
    expect(await verifyFileContent(file("notes.txt", '{"a": 1}'))).toBeNull();
    expect(await verifyFileContent(file("notes.md", `${BOM}# Heading`))).toBeNull();
  });

  it("accepts XML with no declaration to match", async () => {
    // Valid XML, but nothing for a signature to match, so it has to pass on
    // the strength of being text.
    expect(await verifyFileContent(file("data.xml", "<root><a>1</a></root>"))).toBeNull();
  });

  it("rejects text containing NUL bytes as binary", async () => {
    expect(await verifyFileContent(file("a.txt", new Uint8Array([0x68, 0x00, 0x69])))).toContain(
      "isn't readable as text",
    );
  });

  it("accepts multi-byte UTF-8 split by the sniff boundary", async () => {
    // 3 KB of padding then a 3-byte character straddling the read limit; a
    // non-streaming decode would call this invalid UTF-8 and reject it.
    const pad = "a".repeat(3071);
    expect(await verifyFileContent(file("a.txt", `${pad}日本語`))).toBeNull();
  });

  it("rejects an unsupported extension, listing what is allowed", async () => {
    // video/mp4 is deliberately absent from the pipeline's supported types, so
    // this must not upload -- it would sit QUEUED with nothing able to index it.
    const err = await verifyFileContent(file("clip.mp4", PNG_BYTES));
    expect(err).toContain(".mp4 files aren't supported");
    expect(err).toContain("pdf");
  });

  it("rejects an unsupported extension without reading the file", async () => {
    const f = file("clip.mp4", PNG_BYTES);
    const slice = vi.spyOn(f, "slice");
    expect(await verifyFileContent(f)).toContain("aren't supported");
    expect(slice).not.toHaveBeenCalled();
  });

  it("rejects a file with no extension at all", async () => {
    expect(await verifyFileContent(file("README", "some prose"))).toContain("without an extension");
  });

  it("accepts .xml and .text, which the pipeline supports", async () => {
    // Both were missing from the UI accept-list while the server and pipeline
    // took them, so they are pinned here.
    expect(await verifyFileContent(file("feed.xml", '<?xml version="1.0"?><x/>'))).toBeNull();
    expect(await verifyFileContent(file("notes.text", "some prose"))).toBeNull();
  });

  it("treats XML and HTML as interchangeable markup", async () => {
    expect(await verifyFileContent(file("page.html", '<?xml version="1.0"?><x/>'))).toBeNull();
  });

  it("rejects a file the browser refuses to re-read", async () => {
    // What a browser does once the file has changed on disk: the File's
    // snapshot no longer matches, so every read raises NotReadableError.
    const f = file("notes.txt", "hello");
    vi.spyOn(f, "slice").mockReturnValue({
      arrayBuffer: () => Promise.reject(new DOMException("changed", "NotReadableError")),
    } as unknown as Blob);
    expect(await verifyFileContent(f)).toContain("changed while it was being read");
  });

  it("rejects a file that shrank since it was selected", async () => {
    // `size` is the snapshot value; fewer bytes than that means it is moving.
    const f = file("notes.txt", "hello");
    Object.defineProperty(f, "size", { value: 5000 });
    expect(await verifyFileContent(f)).toContain("changed while it was being read");
  });

  it("still lets a genuine read failure surface", async () => {
    const f = file("notes.txt", "hello");
    vi.spyOn(f, "slice").mockReturnValue({
      arrayBuffer: () => Promise.reject(new Error("disk on fire")),
    } as unknown as Blob);
    await expect(verifyFileContent(f)).rejects.toThrow("disk on fire");
  });

  it("does not block a PDF upload when the sniffing detector fails to load", async () => {
    // A chunk-load failure (offline, a stale chunk 404ing after a deploy,
    // CSP) must read as "nothing to check", not as "walked the trie and
    // found no PDF signature" -- the latter is exactly what rejects a
    // renamed-but-genuine PDF below.
    vi.doMock("magic-bytes.js", () => {
      throw new Error("chunk load failed");
    });
    try {
      expect(await verifyFileContent(file("doc.pdf", "just some prose"))).toBeNull();
    } finally {
      vi.doUnmock("magic-bytes.js");
    }
  });
});
