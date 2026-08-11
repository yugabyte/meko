import { describe, it, expect } from "vitest";
import {
  knowledgeFileExtension,
  isAcceptedKnowledgeFile,
  sanitizeFilenameForMatch,
  dedupNote,
  filterFilesForKnowledgeUploadBatch,
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  KNOWLEDGE_UPLOAD_MAX_BATCH,
} from "@/lib/knowledgeFileUploadRules";

// Derive the expected accepted set from the source constant so the test can't
// drift when the whitelist changes (e.g. an extension is added/removed).
const ACCEPTED_EXTS = KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS.split(",").map((s) =>
  s.trim().toLowerCase(),
);

describe("knowledgeFileExtension", () => {
  it("returns the lowercased suffix from the last dot", () => {
    expect(knowledgeFileExtension("Report.PDF")).toBe(".pdf");
    expect(knowledgeFileExtension("a.tar.gz")).toBe(".gz");
  });
  it("returns '' when there is no dot", () => {
    expect(knowledgeFileExtension("README")).toBe("");
  });
  it("treats a dotfile like filepath.Ext does (.pdf → .pdf)", () => {
    expect(knowledgeFileExtension(".pdf")).toBe(".pdf");
  });
});

describe("isAcceptedKnowledgeFile", () => {
  it("accepts every extension in the accepted list (case-insensitive)", () => {
    for (const ext of ACCEPTED_EXTS) {
      expect(isAcceptedKnowledgeFile(`file${ext}`)).toBe(true);
      expect(isAcceptedKnowledgeFile(`FILE${ext.toUpperCase()}`)).toBe(true);
    }
  });
  it("rejects other / missing extensions", () => {
    expect(isAcceptedKnowledgeFile("a.exe")).toBe(false);
    expect(isAcceptedKnowledgeFile("a.m")).toBe(false); // whole-extension match only, no prefix match
    expect(isAcceptedKnowledgeFile("noext")).toBe(false);
  });
});

describe("sanitizeFilenameForMatch", () => {
  it("replaces every char outside [A-Za-z0-9_.-] with _", () => {
    expect(sanitizeFilenameForMatch("my file (1).pdf")).toBe("my_file__1_.pdf");
    expect(sanitizeFilenameForMatch("keep-._OK")).toBe("keep-._OK");
  });
});

describe("dedupNote", () => {
  it("processed + same name", () => {
    expect(dedupNote("doc.pdf", "doc.pdf", false)).toBe("The document is already processed");
  });
  it("processed + genuinely different stored name names the existing file", () => {
    // sanitize("my file.pdf") === "my_file.pdf", so the existing name must
    // differ even after sanitization to count as a different stored name.
    expect(dedupNote("my file.pdf", "other-doc.pdf", false)).toBe(
      "An identical document (other-doc.pdf) is already processed",
    );
  });
  it("awaiting processing + same name", () => {
    expect(dedupNote("doc.pdf", "doc.pdf", true)).toBe(
      "Already uploaded — adding it to the knowledge base now.",
    );
  });
  it("treats a missing existingName as the same name", () => {
    expect(dedupNote("doc.pdf", undefined, false)).toBe("The document is already processed");
  });
});

describe("filterFilesForKnowledgeUploadBatch", () => {
  // Fake the size instead of allocating real buffers — 40MB of zeros in a
  // unit test buys nothing.
  const fileOfSize = (name: string, sizeMB: number): File => {
    const f = new File([""], name);
    Object.defineProperty(f, "size", { value: sizeMB * 1024 * 1024 });
    return f;
  };

  it("skips files over the default 5MB cap with an upgrade nudge", () => {
    const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(
      [fileOfSize("big.pdf", 40), fileOfSize("small.pdf", 1)],
      0,
    );
    expect(accepted.map((f) => f.name)).toEqual(["small.pdf"]);
    expect(warningParts).toEqual([
      "1 skipped (over 5MB — upgrade your plan for larger file uploads)",
    ]);
  });

  it("honors a tier-supplied cap (Standard: 50MB)", () => {
    const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(
      [fileOfSize("big.pdf", 40), fileOfSize("huge.pdf", 60)],
      0,
      50,
    );
    expect(accepted.map((f) => f.name)).toEqual(["big.pdf"]);
    expect(warningParts).toEqual([
      "1 skipped (over 50MB — upgrade your plan for larger file uploads)",
    ]);
  });

  it("never size-skips when the cap is Infinity (un-capped tiers)", () => {
    const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(
      [fileOfSize("huge.pdf", 500)],
      0,
      Number.POSITIVE_INFINITY,
    );
    expect(accepted.map((f) => f.name)).toEqual(["huge.pdf"]);
    expect(warningParts).toEqual([]);
  });

  it("skips unsupported types and caps by remaining batch slots", () => {
    const files = [
      fileOfSize("notes.exe", 1),
      ...Array.from({ length: KNOWLEDGE_UPLOAD_MAX_BATCH + 1 }, (_, i) =>
        fileOfSize(`doc${i}.pdf`, 1),
      ),
    ];
    const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(files, 0);
    expect(accepted).toHaveLength(KNOWLEDGE_UPLOAD_MAX_BATCH);
    expect(warningParts).toEqual([
      "1 skipped (unsupported type)",
      `1 skipped (${KNOWLEDGE_UPLOAD_MAX_BATCH} files already in progress)`,
    ]);
  });
});
