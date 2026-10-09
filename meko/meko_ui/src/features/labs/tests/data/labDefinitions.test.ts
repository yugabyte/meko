import { afterEach, describe, expect, it, vi } from "vitest";

import { flattenLabStages } from "@/features/labs/lib/labChapters";
import type { LabStageId } from "@/features/labs/types";
import { LAB_DEFINITIONS, getLabDefinition } from "@/features/labs/data/labDefinitions";

// Defaults to enabled so every other test in this file sees the full,
// unfiltered chapter list -- getLabDefinition's own AMP-gating behavior gets
// its dedicated test below, which flips this per-test.
const mockIsAmpEnabled = vi.fn(() => true);
vi.mock("@/lib/amp", () => ({ isAmpEnabled: () => mockIsAmpEnabled() }));

afterEach(() => {
  mockIsAmpEnabled.mockReturnValue(true);
});

// LabStageId is a closed union LabStageSlot switches on
// exhaustively -- a lab missing one silently gets a blank pane for it.
const FIXED_STAGE_IDS: LabStageId[] = [
  "see-the-pr",
  "create-datapack",
  "knowledge-base",
  "memory",
  "conversation",
  "meet-agentk",
  "share-with-agent",
  "configure-agentk",
  "guardrail",
  "run-review",
  "review-findings",
  "save-findings",
  "review-cost",
  "let-it-run",
  "public-runs",
  "read-one-yourself",
  "production-reviews",
  "make-it-yours",
];

describe("LAB_DEFINITIONS", () => {
  it("has at least one lab", () => {
    expect(LAB_DEFINITIONS.length).toBeGreaterThan(0);
  });

  it("has unique lab ids", () => {
    const ids = LAB_DEFINITIONS.map((lab) => lab.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("every lab has at least one chapter, and unique chapter ids", () => {
    for (const lab of LAB_DEFINITIONS) {
      expect(lab.chapters.length).toBeGreaterThan(0);
      const chapterIds = lab.chapters.map((chapter) => chapter.id);
      expect(new Set(chapterIds).size).toBe(chapterIds.length);
      for (const chapter of lab.chapters) {
        expect(chapter.title.trim().length).toBeGreaterThan(0);
        expect(chapter.summary.trim().length).toBeGreaterThan(0);
        expect(chapter.stages.length).toBeGreaterThan(0);
      }
    }
  });

  it("every lab walks exactly the fixed stage set", () => {
    for (const lab of LAB_DEFINITIONS) {
      const stageIds = flattenLabStages(lab.chapters).map((stage) => stage.id);
      expect(new Set(stageIds)).toEqual(new Set(FIXED_STAGE_IDS));
    }
  });

  it("has unique stage ids within each lab, across chapters", () => {
    for (const lab of LAB_DEFINITIONS) {
      const stageIds = flattenLabStages(lab.chapters).map((stage) => stage.id);
      expect(new Set(stageIds).size).toBe(stageIds.length);
    }
  });

  it("every stage has a non-empty title and summary", () => {
    for (const lab of LAB_DEFINITIONS) {
      for (const stage of flattenLabStages(lab.chapters)) {
        expect(stage.title.trim().length).toBeGreaterThan(0);
        expect(stage.summary.trim().length).toBeGreaterThan(0);
      }
    }
  });

  it("every deployable lab has a non-empty demoApp.deployable.agentSlug", () => {
    for (const lab of LAB_DEFINITIONS) {
      if (lab.demoApp.deployable) {
        expect(lab.demoApp.deployable.agentSlug.trim().length).toBeGreaterThan(0);
      }
    }
  });

  it("getLabDefinition resolves a known id and returns undefined for an unknown one", () => {
    const first = LAB_DEFINITIONS[0];
    expect(getLabDefinition(first.id)).toBe(first);
    expect(getLabDefinition("does-not-exist")).toBeUndefined();
  });
});
