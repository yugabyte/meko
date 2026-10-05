// The scope tabs decide which projects a user sees, including whether a
// shared one appears at all, so the narrowing is worth asserting directly.

import { describe, expect, it } from "vitest";
import type { Project } from "@/lib/api/projectsApi";
import {
  PROJECT_SCOPE,
  emptyScopeMessage,
  scopedProjects,
} from "@/features/projects/lib/scopeProjects";

const project = (id: string): Project =>
  ({ project_id: id, project_name: id }) as Project;

const owned = [project("mine-a"), project("mine-b")];
const shared = [project("theirs")];

describe("scopedProjects", () => {
  it("shows both lists on All, owned first", () => {
    expect(scopedProjects(owned, shared, PROJECT_SCOPE.all).map((c) => c.project_id)).toEqual(
      ["mine-a", "mine-b", "theirs"],
    );
  });

  it("narrows to one list on the other two tabs", () => {
    expect(scopedProjects(owned, shared, PROJECT_SCOPE.owned)).toEqual(owned);
    expect(scopedProjects(owned, shared, PROJECT_SCOPE.shared)).toEqual(shared);
  });

  it("does not mutate the arrays it was given", () => {
    scopedProjects(owned, shared, PROJECT_SCOPE.all);
    expect(owned).toHaveLength(2);
    expect(shared).toHaveLength(1);
  });
});

describe("emptyScopeMessage", () => {
  it("names the tab rather than giving one message for every case", () => {
    expect(emptyScopeMessage(PROJECT_SCOPE.owned, false)).toContain("not created");
    expect(emptyScopeMessage(PROJECT_SCOPE.shared, false)).toContain("shared with you");
  });

  it("blames the search first, because that is what the user just did", () => {
    expect(emptyScopeMessage(PROJECT_SCOPE.owned, true)).toContain("match your search");
  });
});
