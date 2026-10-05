// "Unfiled" is defined by ABSENCE from the paths map, not by a flag on the
// datapack — these cover that rule and the filter values around it.

import { describe, expect, it } from "vitest";
import type { Datapack } from "@/lib/api/mekoApi";
import type { DatapackPathMap } from "@/lib/api/projectsApi";
import {
  PROJECT_FILTER_ALL,
  PROJECT_FILTER_UNFILED,
  filterDatapacksByProject,
} from "./filterDatapacksByProject";

const dp = (id: string): Datapack =>
  ({ datapack_id: id, datapack_name: id, user_id: "u-1" }) as Datapack;

const FILED = dp("filed");
const ALSO_FILED = dp("also-filed");
const UNFILED = dp("unfiled");
const ALL = [FILED, ALSO_FILED, UNFILED];

const path = (datapackId: string, projectId: string) => ({
  datapack_id: datapackId,
  project_id: projectId,
  project_name: projectId,
  folder_id: `${datapackId}-folder`,
  folder_name: "features",
  path: `${projectId}/features`,
});

const PATHS: DatapackPathMap = {
  filed: path("filed", "proj-1"),
  "also-filed": path("also-filed", "proj-2"),
};

describe("filterDatapacksByProject", () => {
  it("returns everything for the all filter", () => {
    expect(filterDatapacksByProject(ALL, PATHS, PROJECT_FILTER_ALL)).toEqual(ALL);
  });

  it("returns only datapacks missing from the paths map when filtering unfiled", () => {
    expect(filterDatapacksByProject(ALL, PATHS, PROJECT_FILTER_UNFILED)).toEqual([UNFILED]);
  });

  it("narrows to one project", () => {
    expect(filterDatapacksByProject(ALL, PATHS, "proj-1")).toEqual([FILED]);
  });

  it("treats an empty paths map as everything being unfiled", () => {
    expect(filterDatapacksByProject(ALL, {}, PROJECT_FILTER_UNFILED)).toEqual(ALL);
    expect(filterDatapacksByProject(ALL, {}, "proj-1")).toEqual([]);
  });

  it("returns an empty list for a project with nothing filed into it", () => {
    expect(filterDatapacksByProject(ALL, PATHS, "proj-empty")).toEqual([]);
  });
});
