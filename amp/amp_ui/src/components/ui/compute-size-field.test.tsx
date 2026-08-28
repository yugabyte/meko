import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { ComputeSizeField, DEFAULT_COMPUTE_PRESETS } from "@/components/ui/compute-size-field";

// ComputeSizeField is reused by create / provision / resize, so its two jobs
// must hold: a preset click emits that preset's {cpu, memory}, and Custom lets
// the user type an arbitrary size. A regression here would silently send the
// wrong compute reservation to the controller.
afterEach(cleanup);

describe("ComputeSizeField", () => {
  it("emits the preset's cpu/memory when a preset is clicked", () => {
    const onChange = vi.fn();
    render(<ComputeSizeField value={{ cpu: "2", memory: "16Gi" }} onChange={onChange} />);

    fireEvent.click(screen.getByRole("button", { name: /medium/i }));
    expect(onChange).toHaveBeenCalledWith({ cpu: "4", memory: "32Gi" });

    fireEvent.click(screen.getByRole("button", { name: /large/i }));
    expect(onChange).toHaveBeenCalledWith({ cpu: "8", memory: "64Gi" });
  });

  it("marks the preset matching the current value as selected", () => {
    const medium = DEFAULT_COMPUTE_PRESETS.find((p) => p.id === "medium")!;
    render(<ComputeSizeField value={{ cpu: medium.cpu, memory: medium.memory }} onChange={vi.fn()} />);
    expect(screen.getByRole("button", { name: /medium/i }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: /small/i }).getAttribute("aria-pressed")).toBe("false");
  });

  it("reveals cpu/memory inputs in Custom mode and emits typed values", () => {
    const onChange = vi.fn();
    render(<ComputeSizeField value={{ cpu: "2", memory: "16Gi" }} onChange={onChange} />);

    // No custom inputs until Custom is chosen (value matches a preset).
    expect(screen.queryByLabelText(/vcpu/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /custom/i }));
    const cpu = screen.getByLabelText(/vcpu/i);
    const mem = screen.getByLabelText(/memory/i);
    expect(cpu).toBeTruthy();
    expect(mem).toBeTruthy();

    fireEvent.change(cpu, { target: { value: "12" } });
    expect(onChange).toHaveBeenCalledWith({ cpu: "12", memory: "16Gi" });

    fireEvent.change(mem, { target: { value: "32Gi" } });
    expect(onChange).toHaveBeenCalledWith({ cpu: "2", memory: "32Gi" });
  });

  // A custom memory is the only way an unit-less quantity reaches the controller
  // from here, and it would be a 400 (or, before meko#492 was fixed, a four-byte
  // reservation). Flag the field rather than letting the caller submit it.
  it("flags a custom memory with no unit", () => {
    render(<ComputeSizeField value={{ cpu: "2", memory: "4" }} onChange={vi.fn()} />);
    const mem = screen.getByLabelText(/memory/i);
    expect(mem.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText(/needs a unit/i)).toBeTruthy();

    cleanup();
    // Not a preset, or the custom inputs this asserts on are not rendered at all.
    render(<ComputeSizeField value={{ cpu: "2", memory: "20Gi" }} onChange={vi.fn()} />);
    expect(screen.getByLabelText(/memory/i).getAttribute("aria-invalid")).toBe("false");
    expect(screen.queryByText(/needs a unit/i)).toBeNull();
  });
});
