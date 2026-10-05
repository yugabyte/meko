// Searching by FOLDER name is the point of this helper — a user remembers
// "features", not which project it was in.

import { describe, expect, it } from "vitest";
import type { Project } from "@/lib/api/projectsApi";
import { filterProjects } from "./filterProjects";

const project = (name: string, folders: string[]): Project =>
  ({ project_id: name, project_name: name, folder_names: folders }) as Project;

const ADLC = project("adlc", ["coding_standards", "features", "integration_tests"]);
const SUPPORT = project("support", ["playbooks", "escalations"]);
const ALL = [ADLC, SUPPORT];

describe("filterProjects", () => {
  it("returns everything for an empty query", () => {
    expect(filterProjects(ALL, "  ")).toHaveLength(2);
  });

  it("matches on the project name", () => {
    const got = filterProjects(ALL, "support");
    expect(got.map((m) => m.project.project_name)).toEqual(["support"]);
  });

  it("matches on a folder name and reports which folder matched", () => {
    const got = filterProjects(ALL, "features");
    expect(got).toHaveLength(1);
    expect(got[0].project.project_name).toBe("adlc");
    expect(got[0].matchedFolders).toEqual(["features"]);
  });

  it("is case-insensitive and matches partials", () => {
    expect(filterProjects(ALL, "ESCAL")).toHaveLength(1);
  });

  it("reports every folder that matched, not just the first", () => {
    const got = filterProjects([project("p", ["qa-unit", "qa-e2e"])], "qa");
    expect(got[0].matchedFolders).toEqual(["qa-unit", "qa-e2e"]);
  });

  it("leaves matchedFolders empty when only the project name matched", () => {
    expect(filterProjects(ALL, "adlc")[0].matchedFolders).toEqual([]);
  });

  it("returns nothing when neither name nor folders match", () => {
    expect(filterProjects(ALL, "nope")).toEqual([]);
  });

  it("tolerates a project with no folders", () => {
    expect(filterProjects([project("empty", [])], "empty")).toHaveLength(1);
  });
});
