import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { KbHit, MemoryHit } from "@/lib/inferenceGateway";
import { ReviewRetrievalSources } from "./ReviewRetrievalSources";

afterEach(cleanup);

const KB: KbHit[] = [
  { document_id: "d1", document_name: "org-coding-standards.md", chunk_text: "Never build SQL by concatenation.", score: 0.82 },
  { document_id: "d1", document_name: "org-coding-standards.md", chunk_text: "Scope every query by tenant.", score: 0.61 },
  { document_id: "d2", document_name: "api-guidelines.md", chunk_text: "Validate pagination params.", score: 0.4 },
];

describe("ReviewRetrievalSources", () => {
  it("groups chunks under the document they came from", () => {
    render(<ReviewRetrievalSources kbHits={KB} memoryHits={[]} />);
    // Two documents, not three chunk headings.
    expect(screen.getByText("org-coding-standards.md")).toBeTruthy();
    expect(screen.getByText("api-guidelines.md")).toBeTruthy();
    expect(screen.getByText("2 chunks")).toBeTruthy();
    expect(screen.getByText("1 chunk")).toBeTruthy();
  });

  it("shows each chunk's own text, which is the whole point of the section", () => {
    render(<ReviewRetrievalSources kbHits={KB} memoryHits={[]} />);
    expect(screen.getByText("Never build SQL by concatenation.")).toBeTruthy();
    expect(screen.getByText("Scope every query by tenant.")).toBeTruthy();
  });

  // 1 - cosine distance lands at 0-1% for hits that plainly drove a comment,
  // because the distances cluster at ~0.99. No percentage is shown at all.
  it("shows no similarity score", () => {
    render(<ReviewRetrievalSources kbHits={KB} memoryHits={[{ kind: "mem", text: "x", score: 0.7 }]} />);
    expect(screen.queryByText(/% match/)).toBeNull();
  });

  it("labels a memory by how it's held, not by its raw kind", () => {
    const memories: MemoryHit[] = [
      { kind: "mem", text: "We squash-merge.", score: 0.7 },
      { kind: "promoted", text: "Parameterize every query.", score: 0.9 },
    ];
    render(<ReviewRetrievalSources kbHits={[]} memoryHits={memories} />);
    expect(screen.getByText("private")).toBeTruthy();
    expect(screen.getByText("collective")).toBeTruthy();
  });

  it("says nothing matched rather than showing an empty list", () => {
    render(<ReviewRetrievalSources kbHits={[]} memoryHits={[]} />);
    expect(screen.getByText(/didn't match any chunk/)).toBeTruthy();
    expect(screen.getByText(/didn't match any memory/)).toBeTruthy();
  });
});
