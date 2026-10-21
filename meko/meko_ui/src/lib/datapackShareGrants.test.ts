// Viewer-grant detection, which the datapack page and KB file panel use to hide
// the members tile and the "by" row. Other surfaces rely on the API's redaction.
// Also the grant gates that decide whether the create-cluster (owner-only) and
// KB-upload (owner or maintainer) controls render (MEKO-959).
import { describe, expect, it } from "vitest";

import {
  isDatapackGrantEligibleToAddKnowledgeBase,
  isDatapackGrantEligibleToCreateCluster,
  isDatapackGrantEligibleToManageLinkSharing,
  isDatapackGrantViewer,
} from "@/lib/datapackShareGrants";

describe("isDatapackGrantViewer", () => {
  it("is true only for the viewer grant, whatever its case or padding", () => {
    expect(isDatapackGrantViewer({ grant: "viewer" })).toBe(true);
    expect(isDatapackGrantViewer({ grant: " Viewer " })).toBe(true);
  });

  it("is false for every other grant and for an unknown one", () => {
    for (const grant of ["owner", "maintainer", "contributor", "", null, undefined]) {
      expect(isDatapackGrantViewer({ grant })).toBe(false);
    }
  });
});

describe("isDatapackGrantEligibleToCreateCluster", () => {
  it("is true only for the owner", () => {
    expect(isDatapackGrantEligibleToCreateCluster({ grant: "owner" })).toBe(true);
  });

  it("is false for maintainer, contributor, viewer, and unknown", () => {
    for (const grant of ["maintainer", "contributor", "viewer", null, undefined] as const) {
      expect(isDatapackGrantEligibleToCreateCluster({ grant })).toBe(false);
    }
  });
});

describe("isDatapackGrantEligibleToAddKnowledgeBase", () => {
  it("is true for owner and maintainer", () => {
    expect(isDatapackGrantEligibleToAddKnowledgeBase({ grant: "owner" })).toBe(true);
    expect(isDatapackGrantEligibleToAddKnowledgeBase({ grant: "maintainer" })).toBe(true);
  });

  it("is false for contributor and viewer (the MEKO-959 gate)", () => {
    for (const grant of ["contributor", "viewer", null, undefined] as const) {
      expect(isDatapackGrantEligibleToAddKnowledgeBase({ grant })).toBe(false);
    }
  });
});

describe("isDatapackGrantEligibleToManageLinkSharing", () => {
  it("is true for owner only — a share link can reach anyone and resetting it changes the URL for every joiner", () => {
    expect(isDatapackGrantEligibleToManageLinkSharing({ grant: "owner" })).toBe(true);
  });

  it("is false for maintainer, contributor, viewer, and unknown — maintainers still invite explicitly via isDatapackGrantEligibleToShare", () => {
    for (const grant of ["maintainer", "contributor", "viewer", null, undefined] as const) {
      expect(isDatapackGrantEligibleToManageLinkSharing({ grant })).toBe(false);
    }
  });
});
