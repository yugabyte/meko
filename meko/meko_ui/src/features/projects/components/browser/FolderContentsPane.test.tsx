// The open action and the row select are two different intents sharing one
// row, and the row handles Enter/Space itself. These cover the separation in
// both directions — a mouse click and a keyboard Enter on the link must
// navigate WITHOUT also selecting.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { FolderContentsPane } from "@/features/projects/components/browser/FolderContentsPane";
import { OPEN_DATAPACK_LABEL } from "@/features/projects/projectsCopy";
import type { ProjectDatapackNode, ProjectFolder } from "@/lib/api/projectsApi";

const folder: ProjectFolder = {
  folder_id: "f1",
  project_id: "p1",
  folder_name: "cutover_runbooks",
  datapack_count: 1,
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

const node: ProjectDatapackNode = {
  datapack_id: "dp-1",
  datapack_name: "cutover-checklist",
  folder_id: "f1",
  updated_at: new Date().toISOString(),
  memory_count: 20,
  awaiting_review_count: 5,
  promoted_count: 0,
  knowledge_count: 0,
};

function renderPane(onSelectDatapack = vi.fn()) {
  render(
    <MemoryRouter>
      <FolderContentsPane
        folder={folder}
        datapacks={[node]}
        query=""
        onQueryChange={vi.fn()}
        page={1}
        onPageChange={vi.fn()}
        selectedDatapackId={undefined}
        onSelectDatapack={onSelectDatapack}
        isOwner
        onAddDatapacks={vi.fn()}
      />
    </MemoryRouter>,
  );
  return onSelectDatapack;
}

describe("FolderContentsPane open action", () => {
  it("clicking the row selects it rather than navigating", () => {
    const onSelect = renderPane();
    fireEvent.click(screen.getByText("cutover-checklist"));
    expect(onSelect).toHaveBeenCalledWith("dp-1");
  });

  it("clicking open navigates without selecting the row", () => {
    const onSelect = renderPane();
    const open = screen.getByRole("link", {
      name: `${OPEN_DATAPACK_LABEL}: cutover-checklist`,
    });
    expect(open.getAttribute("href")).toBe("/datapacks/dp-1");
    fireEvent.click(open);
    expect(onSelect).not.toHaveBeenCalled();
  });

  // The row's onKeyDown handles Enter and preventDefault()s. stopPropagation
  // on onClick alone does not cover keydown, so without stopping it on the
  // link a keyboard user selected the row instead of opening the datapack.
  it("pressing Enter on open does not select the row", () => {
    const onSelect = renderPane();
    const open = screen.getByRole("link", {
      name: `${OPEN_DATAPACK_LABEL}: cutover-checklist`,
    });
    fireEvent.keyDown(open, { key: "Enter", bubbles: true });
    expect(onSelect).not.toHaveBeenCalled();
  });
});
