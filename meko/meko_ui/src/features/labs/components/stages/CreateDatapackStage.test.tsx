import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { Datapack } from "@/lib/api/mekoApi";
import { CreateDatapackStage } from "./CreateDatapackStage";

afterEach(cleanup);

const DATAPACKS: Datapack[] = [
  { datapack_id: "dp-1", datapack_name: "Alpha", user_id: "u1" },
  { datapack_id: "dp-2", datapack_name: "Beta", user_id: "u1" },
];

function renderStage(overrides: Partial<Parameters<typeof CreateDatapackStage>[0]> = {}) {
  const onSelect = vi.fn();
  const onCreate = vi.fn().mockResolvedValue(undefined);
  const onComplete = vi.fn();
  render(
    <CreateDatapackStage
      datapacks={DATAPACKS}
      loading={false}
      selectedId={null}
      onSelect={onSelect}
      onCreate={onCreate}
      creating={false}
      hasHitDatapackCount={false}
      onComplete={onComplete}
      {...overrides}
    />,
  );
  return { onSelect, onCreate, onComplete };
}

describe("CreateDatapackStage", () => {
  // Advancing is the runner footer's job now; this stage only reports that a
  // datapack was actually chosen, which is what releases the gate.
  it("does not report completion before a datapack is chosen", () => {
    const { onComplete } = renderStage({ selectedId: null });
    expect(onComplete).not.toHaveBeenCalled();
  });

  it("frames the three context sources the next lessons fill", () => {
    renderStage({ selectedId: "dp-1" });
    expect(screen.getByText("Knowledge Base")).toBeTruthy();
    expect(screen.getByText("Memory")).toBeTruthy();
    expect(screen.getByText("Conversation")).toBeTruthy();
  });

  it("creating a new datapack calls onCreate with the trimmed name", async () => {
    const { onCreate } = renderStage();
    fireEvent.click(screen.getByRole("button", { name: /new/i }));
    fireEvent.change(screen.getByLabelText("New datapack name"), { target: { value: "  My Pack  " } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await Promise.resolve();
    expect(onCreate).toHaveBeenCalledWith("My Pack");
  });

  it("renders no heading/description by default, but shows them when passed (a later chapter's reuse)", () => {
    const { rerender } = (() => {
      const onSelect = vi.fn();
      const onCreate = vi.fn().mockResolvedValue(undefined);
      const onComplete = vi.fn();
      const utils = render(
        <CreateDatapackStage
          datapacks={DATAPACKS}
          loading={false}
          selectedId={null}
          onSelect={onSelect}
          onCreate={onCreate}
          creating={false}
          hasHitDatapackCount={false}
          onComplete={onComplete}
        />,
      );
      return utils;
    })();
    expect(screen.queryByText("Choose a datapack")).toBeNull();

    rerender(
      <CreateDatapackStage
        datapacks={DATAPACKS}
        loading={false}
        selectedId={null}
        onSelect={vi.fn()}
        onCreate={vi.fn()}
        creating={false}
        hasHitDatapackCount={false}
        onComplete={vi.fn()}
        heading="Choose a datapack"
        description="Keep the same one, or switch."
      />,
    );
    expect(screen.getByText("Choose a datapack")).toBeTruthy();
    expect(screen.getByText("Keep the same one, or switch.")).toBeTruthy();
  });

  it("disables New once the datapack quota is hit", () => {
    renderStage({ hasHitDatapackCount: true });
    expect((screen.getByRole("button", { name: /new/i }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("surfaces onCreate's rejection instead of silently doing nothing", async () => {
    const onCreate = vi.fn().mockRejectedValue(new Error("You've hit the Free tier limit."));
    renderStage({ onCreate });
    fireEvent.click(screen.getByRole("button", { name: /new/i }));
    fireEvent.change(screen.getByLabelText("New datapack name"), { target: { value: "My Pack" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText("You've hit the Free tier limit.")).toBeTruthy();
    // The dialog stays open with the name intact so the user can retry.
    expect(screen.getByLabelText("New datapack name")).toBeTruthy();
  });
});
