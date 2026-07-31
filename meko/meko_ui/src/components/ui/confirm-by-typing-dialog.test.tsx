import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { ConfirmByTypingDialog } from "./confirm-by-typing-dialog";

afterEach(cleanup);

function openDialog(onConfirm = vi.fn()) {
  render(
    <ConfirmByTypingDialog
      open
      onOpenChange={() => {}}
      title="Delete cluster orders?"
      confirmPhrase="orders"
      confirmLabel="Delete cluster"
      onConfirm={onConfirm}
    />,
  );
  return { onConfirm };
}

describe("ConfirmByTypingDialog", () => {
  it("keeps the destructive action disabled until the exact phrase is typed", () => {
    const { onConfirm } = openDialog();
    const btn = screen.getByRole("button", { name: "Delete cluster" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);

    const input = screen.getByLabelText("Type to confirm");
    fireEvent.change(input, { target: { value: "order" } }); // near miss
    expect(btn.disabled).toBe(true);

    fireEvent.change(input, { target: { value: "orders" } });
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("does not confirm on Enter when the phrase does not match", () => {
    const { onConfirm } = openDialog();
    const input = screen.getByLabelText("Type to confirm");
    fireEvent.change(input, { target: { value: "wrong" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onConfirm).not.toHaveBeenCalled();
  });
});
