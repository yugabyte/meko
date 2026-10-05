// Which projects the All / Mine / Shared with me tabs show.
//
// Mirrors the Datapacks page's scope tabs so the two lists behave the same way,
// but the input differs: GET /projects already returns owned and shared as
// two arrays, so this narrows and concatenates rather than inspecting a grant
// on each row.

import type { Project } from "@/lib/api/projectsApi";

export const PROJECT_SCOPE = {
  all: "all",
  owned: "owned",
  shared: "shared",
} as const;

export type ProjectScope = (typeof PROJECT_SCOPE)[keyof typeof PROJECT_SCOPE];

export function scopedProjects(
  owned: Project[],
  shared: Project[],
  scope: ProjectScope,
): Project[] {
  switch (scope) {
    case PROJECT_SCOPE.owned:
      return owned;
    case PROJECT_SCOPE.shared:
      return shared;
    default:
      // Owned first: on the All tab your own projects are the ones you act
      // on, and a long shared list should not push them below the fold.
      return [...owned, ...shared];
  }
}

/** Empty-state copy that names the reason the grid is empty, rather than one
 *  message that is wrong in two of the three cases. */
export function emptyScopeMessage(scope: ProjectScope, searchActive: boolean): string {
  if (searchActive) return "No projects or folders match your search.";
  switch (scope) {
    case PROJECT_SCOPE.owned:
      return "You have not created a project yet.";
    case PROJECT_SCOPE.shared:
      return "No projects have been shared with you.";
    default:
      return "No projects yet.";
  }
}
