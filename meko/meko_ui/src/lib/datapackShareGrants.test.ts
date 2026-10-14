// Viewer-grant detection, which the datapack page and KB file panel use to hide
// the members tile and the "by" row. Other surfaces rely on the API's redaction.
import { describe, expect, it } from "vitest";

import { isDatapackGrantViewer } from "@/lib/datapackShareGrants";

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
