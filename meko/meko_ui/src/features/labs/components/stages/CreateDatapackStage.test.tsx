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
  const onContinue = vi.fn();
  render(
    <CreateDatapackStage
      datapacks={DATAPACKS}
      loading={false}
      selectedId={null}
      onSelect={onSelect}
      onCreate={onCreate}
      creating={false}
      hasHitDatapackCount={false}
      onContinue={onContinue}
      {...overrides}
    />,
  );
  return { onSelect, onCreate, onContinue };
}

describe("CreateDatapackStage", () => {
  it("Next is disabled until a datapack is selected", () => {
    renderStage({ selectedId: null });
    expect((screen.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("Next is enabled once a datapack is selected", () => {
    renderStage({ selectedId: "dp-1" });
    expect((screen.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("clicking Next calls onContinue", () => {
    const { onContinue } = renderStage({ selectedId: "dp-1" });
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(onContinue).toHaveBeenCalled();
  });

  it("creating a new datapack calls onCreate with the trimmed name", async () => {
    const { onCreate } = renderStage();
    fireEvent.click(screen.getByRole("button", { name: /new/i }));
    fireEvent.change(screen.getByLabelText("New datapack name"), { target: { value: "  My Pack  " } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await Promise.resolve();
    expect(onCreate).toHaveBeenCalledWith("My Pack");
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
