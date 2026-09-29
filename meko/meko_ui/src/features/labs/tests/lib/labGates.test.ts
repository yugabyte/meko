import { describe, expect, it } from "vitest";

import type { LabContextFeed } from "@/features/labs/hooks/useLabContextFeed";
import { stagesSatisfiedByContext } from "@/features/labs/lib/labGates";

function feed(overrides: Partial<LabContextFeed> = {}): LabContextFeed {
  return {
    knowledge: [],
    knowledgeTotal: 0,
    memories: [],
    memoriesTotal: 0,
    collective: [],
    collectiveTotal: 0,
    conversationsTotal: 0,
    loading: false,
    ...overrides,
  };
}

describe("stagesSatisfiedByContext", () => {
  it("satisfies nothing for an empty datapack, so every lesson still gates", () => {
    expect(stagesSatisfiedByContext(feed(), null)).toEqual([]);
  });

  // The bug this exists for: a user whose standards doc was already indexed
  // never fires the lesson's trace, and before this could not leave the page
  // without uploading a second copy.
  it("satisfies the knowledge lesson when the datapack already has a document", () => {
    expect(stagesSatisfiedByContext(feed({ knowledgeTotal: 1 }), null)).toContain("knowledge-base");
  });

  it("satisfies the memory lesson when the datapack already has a memory", () => {
    expect(stagesSatisfiedByContext(feed({ memoriesTotal: 3 }), null)).toContain("memory");
  });

  it("satisfies the conversation lesson when the datapack already has a conversation", () => {
    expect(stagesSatisfiedByContext(feed({ conversationsTotal: 1 }), null)).toContain("conversation");
  });

  it("keeps the three independent — knowledge present doesn't unlock the others", () => {
    expect(stagesSatisfiedByContext(feed({ knowledgeTotal: 4 }), null)).toEqual(["knowledge-base"]);
  });

  it("ignores collective memory, which no lesson in chapter 1 produces", () => {
    expect(stagesSatisfiedByContext(feed({ collectiveTotal: 5 }), null)).toEqual([]);
  });

  // Clearing lab progress re-locks every gate, but the datapack is still
  // selected -- re-picking it to unlock Next would be redoing what's done.
  it("satisfies the datapack lesson whenever one is already selected", () => {
    expect(stagesSatisfiedByContext(feed(), "dp-1")).toEqual(["create-datapack"]);
  });
});
