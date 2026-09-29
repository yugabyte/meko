import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { KbHit, MemoryHit } from "@/lib/inferenceGateway";
import { ReviewRetrievalSources } from "@/features/labs/components/demo-apps/ReviewRetrievalSources";

afterEach(cleanup);

const KB: KbHit[] = [
  {
    document_id: "d1",
    document_name: "org-coding-standards.md",
    chunk_text: "Never build SQL by concatenation.",
    score: 0.82,
  },
  {
    document_id: "d1",
    document_name: "org-coding-standards.md",
    chunk_text: "Scope every query by tenant.",
    score: 0.61,
  },
  {
    document_id: "d2",
    document_name: "api-guidelines.md",
    chunk_text: "Validate pagination params.",
    score: 0.4,
  },
];

describe("ReviewRetrievalSources", () => {
  it("quotes every chunk, which is the whole point of the section", () => {
    render(<ReviewRetrievalSources kbHits={KB} memoryHits={[]} />);
    expect(screen.getByText(/Never build SQL by concatenation/)).toBeTruthy();
    expect(screen.getByText(/Scope every query by tenant/)).toBeTruthy();
    expect(screen.getByText(/Validate pagination params/)).toBeTruthy();
  });

  // A chunk with no source is a rule the reader can't go and fix.
  it("names the document each chunk came from", () => {
    render(<ReviewRetrievalSources kbHits={KB} memoryHits={[]} />);
    expect(screen.getAllByText("org-coding-standards.md")).toHaveLength(2);
    expect(screen.getByText("api-guidelines.md")).toBeTruthy();
  });

  // 1 - cosine distance lands at 0-1% for hits that plainly drove a comment,
  // because the distances cluster at ~0.99. No percentage is shown at all.
  it("shows no similarity score", () => {
    render(
      <ReviewRetrievalSources kbHits={KB} memoryHits={[{ kind: "mem", text: "x", score: 0.7 }]} />,
    );
    expect(screen.queryByText(/% match/)).toBeNull();
  });

  it("labels a memory by how it's held, not by its raw kind", () => {
    const memories: MemoryHit[] = [
      { kind: "mem", text: "We squash-merge.", score: 0.7 },
      { kind: "promoted", text: "Parameterize every query.", score: 0.9 },
    ];
    render(<ReviewRetrievalSources kbHits={[]} memoryHits={memories} />);
    expect(screen.getByText("private memory")).toBeTruthy();
    expect(screen.getByText("collective memory")).toBeTruthy();
  });

  // Retrieving nothing is a result, not a blank space.
  it("says the comments were the model's own when nothing matched", () => {
    render(<ReviewRetrievalSources kbHits={[]} memoryHits={[]} />);
    expect(screen.getByText(/every comment above is the model on its own/)).toBeTruthy();
  });
});
