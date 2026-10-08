// The browser opens on the first folder, not the project, so "Add a datapack"
// is on screen without a click. That is a product decision a refactor of the
// selection state could silently undo, and the fallback for a project with no
// folders yet is the case it would undo first.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { ProjectBrowser } from "@/features/projects/components/browser/ProjectBrowser";
import { ADD_A_DATAPACK_LABEL } from "@/features/projects/projectsCopy";
import type { ProjectDetail, ProjectFolder } from "@/lib/api/projectsApi";

const now = new Date().toISOString();

function folder(id: string, name: string): ProjectFolder {
  return {
    folder_id: id,
    project_id: "p1",
    folder_name: name,
    datapack_count: 0,
    created_at: now,
    updated_at: now,
  };
}

// Alphabetical, the order the API sorts folders into and the order the rail
// renders them in, so folders[0] is the row at the top of the list.
function detail(folders: ProjectFolder[]): ProjectDetail {
  return {
    project: {
      project_id: "p1",
      user_id: "u1",
      project_name: "adlc_dev",
      theme_key: "custom",
      theme_version: "1",
      folder_count: folders.length,
      datapack_count: 0,
      created_at: now,
      updated_at: now,
    },
    folders,
    datapacks: [],
    members: { people: 1, agents: 0 },
  };
}

function renderBrowser(folders: ProjectFolder[]) {
  const client = new QueryClient();
  const tree = (f: ProjectFolder[]) => (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ProjectBrowser
          detail={detail(f)}
          isOwner
          ownDatapacks={[]}
          datapackPaths={{}}
          learningsCount={0}
        />
      </MemoryRouter>
    </QueryClientProvider>
  );
  const r = render(tree(folders));
  // Re-render the SAME instance, so state is kept -- a fresh mount would
  // re-run the initializer and prove nothing about the folders-arrive case.
  return { rerender: (f: ProjectFolder[]) => r.rerender(tree(f)) };
}

describe("ProjectBrowser initial selection", () => {
  it("opens the first folder, so adding a datapack takes no click to reach", () => {
    renderBrowser([folder("f1", "coding_standards"), folder("f2", "features")]);

    // The contents pane only renders for an open folder, and its header names
    // the folder it belongs to.
    expect(screen.getByRole("button", { name: ADD_A_DATAPACK_LABEL })).toBeTruthy();
    expect(screen.getAllByText("coding_standards").length).toBeGreaterThan(0);
  });

  it("falls back to the project when there are no folders to open", () => {
    renderBrowser([]);

    // Nothing to add a datapack to yet, so the contents pane stays closed
    // rather than rendering an empty folder.
    expect(screen.queryByRole("button", { name: ADD_A_DATAPACK_LABEL })).toBeNull();
  });

  it("opens the first folder the moment one is created", () => {
    // The useState initializer runs once, so a project that mounted empty
    // would otherwise stay on the project after its first folder arrives --
    // the exact state this change exists to avoid, on a brand new project.
    const { rerender } = renderBrowser([]);
    expect(screen.queryByRole("button", { name: ADD_A_DATAPACK_LABEL })).toBeNull();

    rerender([folder("f1", "coding_standards")]);

    expect(screen.getByRole("button", { name: ADD_A_DATAPACK_LABEL })).toBeTruthy();
  });
});
