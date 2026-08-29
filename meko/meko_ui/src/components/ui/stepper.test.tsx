import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { Stepper } from "./stepper";

afterEach(cleanup);

describe("Stepper", () => {
  it("clicking a step calls onSelect with that index", () => {
    const onSelect = vi.fn();
    render(<Stepper steps={["One", "Two", "Three"]} current={0} onSelect={onSelect} />);

    fireEvent.click(screen.getByText("Two"));

    expect(onSelect).toHaveBeenCalledWith(1);
  });

  it("omitting onSelect renders no interactive button role (existing presentational usage is unaffected)", () => {
    render(<Stepper steps={["One", "Two", "Three"]} current={0} />);

    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });
});
