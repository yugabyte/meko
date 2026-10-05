// Narrows a project list by a search term, matching the project's name OR any
// of its folder names.
//
// Runs in the browser against data the page already has: GET /projects returns
// folder_names on every project, so folder search costs no request and needs no
// index. It is also leak-safe on shared projects, because folder_names there is
// already scoped to folders the caller can reach.

import type { Project } from "@/lib/api/projectsApi";

/** Whether this project matches, and why — so the UI can show the folder that
 *  caused a match rather than leaving the user guessing. */
export type ProjectMatch = {
  project: Project;
  /** Folder names that matched. Empty when only the project name did. */
  matchedFolders: string[];
};

export function filterProjects(projects: Project[], query: string): ProjectMatch[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return projects.map((project) => ({ project, matchedFolders: [] }));

  const out: ProjectMatch[] = [];
  for (const project of projects) {
    const matchedFolders = (project.folder_names ?? []).filter((name) =>
      name.toLowerCase().includes(needle),
    );
    const nameMatches = project.project_name.toLowerCase().includes(needle);
    if (nameMatches || matchedFolders.length > 0) {
      out.push({ project, matchedFolders });
    }
  }
  return out;
}
