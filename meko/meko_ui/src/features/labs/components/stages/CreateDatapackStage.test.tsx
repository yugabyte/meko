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

  // Creating is the path this step opens on: a lab run writes sample
  // standards and memories, which should not land in a datapack the user
  // already relies on.
  it("opens on the create form rather than the picker", () => {
    renderStage();
    expect(screen.getByLabelText("Create a datapack for this lab")).toBeTruthy();
    expect(screen.queryByLabelText("Select datapack")).toBeNull();
  });

  it("creating a new datapack calls onCreate with the trimmed name", async () => {
    const { onCreate } = renderStage();
    fireEvent.change(screen.getByLabelText("Create a datapack for this lab"), {
      target: { value: "  My Pack  " },
    });
    fireEvent.click(screen.getByRole("button", { name: /create datapack/i }));
    await Promise.resolve();
    expect(onCreate).toHaveBeenCalledWith("My Pack");
  });

  it("reaches the existing-datapack picker from the create form, and back", () => {
    renderStage();
    fireEvent.click(screen.getByRole("button", { name: /use a datapack you already have/i }));
    expect(screen.getByLabelText("Select datapack")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /create a new one/i }));
    expect(screen.getByLabelText("Create a datapack for this lab")).toBeTruthy();
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

  // At the quota there is nothing to create, so the step opens on the picker
  // and offers no way back to a form that could only fail.
  it("opens on the picker with no create option once the datapack quota is hit", () => {
    renderStage({ hasHitDatapackCount: true });
    expect(screen.getByLabelText("Select datapack")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /create a new one/i })).toBeNull();
    expect(screen.getByText(/hit your datapack limit/i)).toBeTruthy();
  });

  it("surfaces onCreate's rejection instead of silently doing nothing", async () => {
    const onCreate = vi.fn().mockRejectedValue(new Error("You've hit the Free tier limit."));
    renderStage({ onCreate });
    fireEvent.change(screen.getByLabelText("Create a datapack for this lab"), {
      target: { value: "My Pack" },
    });
    fireEvent.click(screen.getByRole("button", { name: /create datapack/i }));

    expect(await screen.findByText("You've hit the Free tier limit.")).toBeTruthy();
    // The form stays put with the name intact so the user can retry.
    expect(screen.getByLabelText("Create a datapack for this lab")).toBeTruthy();
  });
});
