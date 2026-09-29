import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { LessonTrace } from "@/features/labs/lib/lessonTrace";
import { XRayPane } from "@/features/labs/components/lab-runner/XRayPane";

afterEach(cleanup);

describe("XRayPane", () => {
  it("shows an empty state before any lesson has completed", () => {
    render(<XRayPane trace={null} />);
    fireEvent.click(screen.getByRole("button", { name: /x-ray/i }));
    expect(screen.getByText(/Nothing recorded in this browser yet/)).toBeTruthy();
  });

  it("is collapsed by default -- the trace is not visible until expanded", () => {
    const trace: LessonTrace = {
      lessonTitle: "Add a memory",
      calls: [{ name: "memory_add", durationMs: 120, status: "ok" }],
    };
    render(<XRayPane trace={trace} />);
    expect(screen.queryByText("memory_add")).toBeNull();
  });

  it("shows each call's name, duration, and status once expanded", () => {
    const trace: LessonTrace = {
      lessonTitle: "Add a memory",
      calls: [
        { name: "memory_add", durationMs: 120, status: "ok" },
        { name: "memory_add", durationMs: 45, status: "error", detail: "Memory limit reached." },
      ],
    };
    render(<XRayPane trace={trace} />);
    fireEvent.click(screen.getByRole("button", { name: /x-ray/i }));

    expect(screen.getByText("Add a memory")).toBeTruthy();
    const names = screen.getAllByText("memory_add");
    expect(names).toHaveLength(2);
    expect(screen.getByText("120ms")).toBeTruthy();
    expect(screen.getByText("45ms")).toBeTruthy();
    expect(screen.getByText("Memory limit reached.")).toBeTruthy();
  });

  it("caps its own height and scrolls internally instead of growing unbounded", () => {
    const trace: LessonTrace = {
      lessonTitle: "Chat about a preference",
      calls: Array.from({ length: 12 }, (_, i) => ({
        name: `step-${i}`,
        durationMs: 10,
        status: "ok" as const,
      })),
    };
    render(<XRayPane trace={trace} />);
    fireEvent.click(screen.getByRole("button", { name: /x-ray/i }));

    expect(screen.getByText("step-0")).toBeTruthy();
    expect(screen.getByText("step-11")).toBeTruthy();
    const list = screen.getByText("step-0").closest("ul");
    expect(list?.className).toMatch(/max-h-\[300px\]/);
    expect(list?.className).toMatch(/overflow-y-auto/);
  });
});
