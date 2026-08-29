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

  it("every lab has at least one chapter, and every chapter at least one step", () => {
    for (const lab of LAB_DEFINITIONS) {
      expect(lab.chapters.length).toBeGreaterThan(0);
      for (const chapter of lab.chapters) {
        expect(chapter.steps.length).toBeGreaterThan(0);
      }
    }
  });

  it("has unique step ids within each lab", () => {
    for (const lab of LAB_DEFINITIONS) {
      const stepIds = lab.chapters.flatMap((chapter) => chapter.steps.map((step) => step.id));
      expect(new Set(stepIds).size).toBe(stepIds.length);
    }
  });

  it("every lab has a non-empty demoApp.datapackName", () => {
    for (const lab of LAB_DEFINITIONS) {
      expect(lab.demoApp.datapackName.trim().length).toBeGreaterThan(0);
    }
  });

  it("getLabDefinition resolves a known id and returns undefined for an unknown one", () => {
    const first = LAB_DEFINITIONS[0];
    expect(getLabDefinition(first.id)).toBe(first);
    expect(getLabDefinition("does-not-exist")).toBeUndefined();
  });
});
