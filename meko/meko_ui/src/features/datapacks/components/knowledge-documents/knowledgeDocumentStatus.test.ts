import { describe, it, expect } from "vitest";
import {
  kbDocumentStatusLabel,
  kbDocumentStatusTone,
  KB_DOCUMENT_STATUS,
} from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentStatus";

describe("kbDocumentStatusLabel", () => {
  it("maps each known status to a title-cased label", () => {
    expect(kbDocumentStatusLabel(KB_DOCUMENT_STATUS.QUEUED)).toBe("Queued");
    expect(kbDocumentStatusLabel(KB_DOCUMENT_STATUS.PROCESSING)).toBe("Processing");
    expect(kbDocumentStatusLabel(KB_DOCUMENT_STATUS.COMPLETED)).toBe("Completed");
    expect(kbDocumentStatusLabel(KB_DOCUMENT_STATUS.FAILED)).toBe("Failed");
  });
  it("passes an unknown status through unchanged", () => {
    expect(kbDocumentStatusLabel("SOMETHING_NEW")).toBe("SOMETHING_NEW");
  });
});

describe("kbDocumentStatusTone", () => {
  it("maps terminal/active states to their tone", () => {
    expect(kbDocumentStatusTone(KB_DOCUMENT_STATUS.COMPLETED)).toBe("green");
    expect(kbDocumentStatusTone(KB_DOCUMENT_STATUS.FAILED)).toBe("red");
    expect(kbDocumentStatusTone(KB_DOCUMENT_STATUS.PROCESSING)).toBe("blue");
  });
  it("falls back to muted for QUEUED and unknown statuses", () => {
    expect(kbDocumentStatusTone(KB_DOCUMENT_STATUS.QUEUED)).toBe("muted");
    expect(kbDocumentStatusTone("WHATEVER")).toBe("muted");
  });
});
