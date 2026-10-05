// The breadcrumb picks one of two trails from `showProject` and `projectPath`,
// and the filed one is what sends a reader back to the project. These pin both
// trails and the flag-off case, which otherwise differ only by a prop.

import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { DatapackDetailHero } from "@/features/datapacks/components/datapack-detail/DatapackDetailHero";
import { PROJECTS_TITLE } from "@/features/projects/projectsCopy";
import { ROUTES, projectDetailPath } from "@/app/routes/paths";
import type { DatapackPath } from "@/lib/api/projectsApi";

const projectPath: DatapackPath = {
  datapack_id: "dp-1",
  project_id: "p1",
  project_name: "adlc",
  folder_id: "f1",
  folder_name: "features",
  path: "adlc/features",
};

function renderHero(props: Partial<React.ComponentProps<typeof DatapackDetailHero>> = {}) {
  render(
    <MemoryRouter>
      <DatapackDetailHero
        datapackName="cutover-checklist"
        datapackGrant={null}
        displayId="dp-1"
        createdLabel="2 days ago"
        statusLoading={false}
        actions={null}
        {...props}
      />
    </MemoryRouter>,
  );
  return within(screen.getByRole("navigation", { name: "Breadcrumb" }));
}

describe("DatapackDetailHero breadcrumb", () => {
  it("retraces the project route when the datapack is filed", () => {
    const trail = renderHero({ showProject: true, projectPath });

    expect(trail.getByRole("link", { name: PROJECTS_TITLE }).getAttribute("href")).toBe(
      ROUTES.projects,
    );
    expect(trail.getByRole("link", { name: "adlc" }).getAttribute("href")).toBe(
      projectDetailPath("p1"),
    );
    // The datapack itself is where we already are, so it is named but not a link.
    expect(trail.queryByRole("link", { name: "cutover-checklist" })).toBeNull();
    expect(trail.getByText("cutover-checklist")).toBeTruthy();
  });

  it("falls back to the flat Datapacks trail when unfiled", () => {
    const trail = renderHero({ showProject: true, projectPath: undefined });

    expect(trail.getByRole("link", { name: "Datapacks" }).getAttribute("href")).toBe(
      ROUTES.datapacks,
    );
    expect(trail.queryByRole("link", { name: PROJECTS_TITLE })).toBeNull();
  });

  it("keeps the project out of the trail when the feature is off", () => {
    // Filed, but the flag is off: showing the project entry would surface a
    // route the reader cannot reach.
    const trail = renderHero({ showProject: false, projectPath });

    expect(trail.getByRole("link", { name: "Datapacks" }).getAttribute("href")).toBe(
      ROUTES.datapacks,
    );
    expect(trail.queryByRole("link", { name: PROJECTS_TITLE })).toBeNull();
    expect(trail.queryByRole("link", { name: "adlc" })).toBeNull();
  });
});
