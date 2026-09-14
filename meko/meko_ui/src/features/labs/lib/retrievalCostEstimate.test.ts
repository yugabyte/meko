import { describe, expect, it } from "vitest";

import {
  costMultiple,
  estimatePasteTokens,
  estimatePasteTokensFromDocuments,
  estimateQueryTokens,
  estimateTokens,
} from "./retrievalCostEstimate";

describe("estimateTokens", () => {
  it("is zero for empty text", () => {
    expect(estimateTokens("")).toBe(0);
  });
});

describe("estimateQueryTokens", () => {
  it("returns zero rather than dividing by a zero chunk count", () => {
    expect(estimateQueryTokens("a".repeat(400), 0)).toBe(0);
  });

  it("never claims to retrieve more chunks than the document has", () => {
    const doc = "a".repeat(400); // 100 tokens
    // One chunk: retrieving "two" can't exceed the whole document.
    expect(estimateQueryTokens(doc, 1)).toBe(estimatePasteTokens(doc));
  });

  it("costs a fraction of pasting once the document is chunked", () => {
    const doc = "a".repeat(4000); // 1000 tokens over 20 chunks
    expect(estimateQueryTokens(doc, 20)).toBeLessThan(estimatePasteTokens(doc));
  });
});

describe("costMultiple", () => {
  it("is null when there is nothing to compare, so callers can hide the claim", () => {
    expect(costMultiple(0, 0)).toBeNull();
    expect(costMultiple(1000, 0)).toBeNull();
  });

  it("reports how many times more input pasting costs", () => {
    expect(costMultiple(2000, 100)).toBe(20);
  });
});

describe("estimatePasteTokensFromDocuments", () => {
  it("is null when nothing has been uploaded, so callers don't print a confident zero", () => {
    expect(estimatePasteTokensFromDocuments([], 1024)).toBeNull();
    expect(estimatePasteTokensFromDocuments([{ size_bytes: null }], 1024)).toBeNull();
  });

  // Chunks are counted AFTER text extraction, so they don't care whether the
  // document arrived as Markdown or as a PDF full of fonts and structure.
  it("prefers the chunk count, which is independent of the file format", () => {
    expect(estimatePasteTokensFromDocuments([{ chunks_processed: 4, size_bytes: 1_000_000 }], 1024)).toBe(
      1024,
    );
  });

  it("sums chunks across every uploaded document", () => {
    const docs = [{ chunks_processed: 2 }, { chunks_processed: 6 }];
    expect(estimatePasteTokensFromDocuments(docs, 1024)).toBe(2048);
  });

  it("falls back to bytes when the index hasn't reported a chunk size", () => {
    expect(estimatePasteTokensFromDocuments([{ chunks_processed: 4, size_bytes: 4000 }], null)).toBe(1000);
  });

  it("falls back to bytes when a document reports no chunks yet", () => {
    expect(estimatePasteTokensFromDocuments([{ size_bytes: 8000 }], 1024)).toBe(2000);
  });
});
