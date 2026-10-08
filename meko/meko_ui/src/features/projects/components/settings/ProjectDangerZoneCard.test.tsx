// The datapack picker in front of an irreversible delete. It is a searchable
// Combobox inside a Dialog -- a Popover nested in a focus trap -- so these
// cover that the search input is reachable at all, that filtering narrows the
// list, and that picking a row arms the type-to-confirm guard.

import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ProjectDangerZoneCard } from "@/features/projects/components/settings/ProjectDangerZoneCard";
import type { Datapack } from "@/lib/api/mekoApi";
import type { Project, ProjectDatapackNode } from "@/lib/api/projectsApi";

afterEach(cleanup);

// Radix's popover positioning needs a ResizeObserver, which jsdom does not
// provide. A no-op is enough: these assert which options are listed, not
// where the popover lands.
beforeAll(() => {
  global.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  // cmdk scrolls the active item into view as you filter; jsdom has no
  // layout, so the method is simply absent.
  Element.prototype.scrollIntoView = vi.fn();
});

const now = new Date().toISOString();

const project: Project = {
  project_id: "p1",
  user_id: "u1",
  project_name: "adlc_dev",
  theme_key: "custom",
  theme_version: "1",
  folder_count: 1,
  datapack_count: 3,
  created_at: now,
  updated_at: now,
};

const NAMES = ["adlc-code-review-Oct7", "adlc-live-demo-India", "feature/tenant-usage-report"];

const nodes: ProjectDatapackNode[] = NAMES.map((name, i) => ({
  datapack_id: `dp-${i}`,
  datapack_name: name,
  folder_id: "f1",
  updated_at: now,
  memory_count: 0,
  awaiting_review_count: 0,
  promoted_count: 0,
  knowledge_count: 0,
}));

// Deletable only when owned, which the card filters on.
const owned = NAMES.map((name, i) => ({ datapack_id: `dp-${i}`, datapack_name: name })) as Datapack[];

function openDeleteDatapackDialog() {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ProjectDangerZoneCard
        project={project}
        datapacks={nodes}
        ownDatapacks={owned}
        onProjectDeleted={vi.fn()}
      />
    </QueryClientProvider>,
  );
  // "Delete a datapack" opens the dialog; "Delete datapack" inside it confirms.
  fireEvent.click(screen.getByRole("button", { name: "Delete a datapack" }));
  // Named by the "Datapack" Label via htmlFor -- role=combobox takes its name
  // from the author, so the placeholder text alone would leave it nameless.
  return screen.getByRole("combobox", { name: "Datapack" });
}

describe("ProjectDangerZoneCard datapack picker", () => {
  it("offers a search box inside the dialog", () => {
    fireEvent.click(openDeleteDatapackDialog());

    expect(screen.getByPlaceholderText("Search datapacks…")).toBeTruthy();
    // All three owned datapacks are listed before any filtering.
    for (const name of NAMES) {
      expect(screen.getByRole("option", { name })).toBeTruthy();
    }
  });

  it("narrows the list as you type, which is the point with hundreds of them", () => {
    fireEvent.click(openDeleteDatapackDialog());
    fireEvent.change(screen.getByPlaceholderText("Search datapacks…"), {
      target: { value: "live-demo" },
    });

    expect(screen.getByRole("option", { name: "adlc-live-demo-India" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "adlc-code-review-Oct7" })).toBeNull();
  });

  it("arms the type-to-confirm guard with the datapack that was picked", () => {
    fireEvent.click(openDeleteDatapackDialog());
    fireEvent.click(screen.getByRole("option", { name: "adlc-live-demo-India" }));

    const confirm = screen.getByRole("button", {
      name: "Delete datapack",
    }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);

    // The phrase is the SELECTED datapack's name, so a different one does not
    // unlock the button -- picking and confirming have to agree.
    const typed = screen.getByLabelText("Type to confirm");
    fireEvent.change(typed, { target: { value: "adlc-code-review-Oct7" } });
    expect(confirm.disabled).toBe(true);

    fireEvent.change(typed, { target: { value: "adlc-live-demo-India" } });
    expect(confirm.disabled).toBe(false);
  });
});
