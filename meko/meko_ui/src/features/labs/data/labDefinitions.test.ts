import { describe, expect, it } from "vitest";

import { flattenLabStages } from "../lib/labChapters";
import type { LabStageId } from "../types";
import { LAB_DEFINITIONS, getLabDefinition } from "./labDefinitions";

// LabStageId is a closed 9-value union LabStageSlot switches on
// exhaustively -- a lab missing one silently gets a blank pane for it.
const FIXED_STAGE_IDS: LabStageId[] = [
  "create-datapack",
  "context-intro",
  "knowledge-base",
  "memory",
  "conversation",
  "deploy-agent",
  "share-with-agent",
  "run-review",
  "collaborate",
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

  it("every lab has exactly the 9 fixed stages", () => {
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

  it("every stage has exactly two non-empty highlights", () => {
    for (const lab of LAB_DEFINITIONS) {
      for (const stage of flattenLabStages(lab.chapters)) {
        expect(stage.highlights.length).toBe(2);
        for (const highlight of stage.highlights) {
          expect(highlight.trim().length).toBeGreaterThan(0);
        }
      }
    }
  });

  it("every lab has exactly three non-empty startingPoint steps", () => {
    for (const lab of LAB_DEFINITIONS) {
      expect(lab.startingPoint.length).toBe(3);
      for (const step of lab.startingPoint) {
        expect(step.title.trim().length).toBeGreaterThan(0);
        expect(step.subtitle.trim().length).toBeGreaterThan(0);
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
