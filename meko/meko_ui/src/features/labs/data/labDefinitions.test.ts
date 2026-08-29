import { describe, expect, it } from "vitest";

import { LAB_DEFINITIONS, getLabDefinition } from "./labDefinitions";

describe("LAB_DEFINITIONS", () => {
  it("has at least one lab", () => {
    expect(LAB_DEFINITIONS.length).toBeGreaterThan(0);
  });

  it("has unique lab ids", () => {
    const ids = LAB_DEFINITIONS.map((lab) => lab.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("every lab has at least the 5 fixed stages", () => {
    for (const lab of LAB_DEFINITIONS) {
      expect(lab.stages.length).toBeGreaterThan(0);
    }
  });

  it("has unique stage ids within each lab", () => {
    for (const lab of LAB_DEFINITIONS) {
      const stageIds = lab.stages.map((stage) => stage.id);
      expect(new Set(stageIds).size).toBe(stageIds.length);
    }
  });

  it("every stage has a non-empty title, shortLabel, and summary", () => {
    for (const lab of LAB_DEFINITIONS) {
      for (const stage of lab.stages) {
        expect(stage.title.trim().length).toBeGreaterThan(0);
        expect(stage.shortLabel.trim().length).toBeGreaterThan(0);
        expect(stage.summary.trim().length).toBeGreaterThan(0);
      }
    }
  });

  it("every stage has exactly two non-empty highlights", () => {
    for (const lab of LAB_DEFINITIONS) {
      for (const stage of lab.stages) {
        expect(stage.highlights.length).toBe(2);
        for (const highlight of stage.highlights) {
          expect(highlight.trim().length).toBeGreaterThan(0);
        }
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
