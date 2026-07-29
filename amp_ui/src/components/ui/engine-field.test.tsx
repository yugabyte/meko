import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { EngineField } from "@/components/ui/engine-field";

// The whole point of EngineField today: PostgreSQL is the only selectable
// engine; every other engine is a disabled "Coming soon" roadmap tile. These
// tests guard that contract so a future edit can't silently make Oracle/etc.
// clickable (which would let a user "create" an unsupported cluster).
afterEach(cleanup);

describe("EngineField", () => {
  it("makes PostgreSQL selectable and every other engine a disabled Coming-soon tile", () => {
    const onChange = vi.fn();
    render(<EngineField value="postgres" onChange={onChange} />);

    const pg = screen.getByRole("button", { name: /postgresql/i });
    expect((pg as HTMLButtonElement).disabled).toBe(false);

    // The four roadmap engines are present, disabled, and labelled Coming soon.
    for (const name of [/ai db/i, /oracle/i, /sql server/i, /documentdb/i]) {
      const btn = screen.getByRole("button", { name });
      expect((btn as HTMLButtonElement).disabled).toBe(true);
    }
    expect(screen.getAllByText(/coming soon/i)).toHaveLength(4);
  });

  it("does not fire onChange when a Coming-soon engine is clicked", () => {
    const onChange = vi.fn();
    render(<EngineField value="postgres" onChange={onChange} />);

    fireEvent.click(screen.getByRole("button", { name: /oracle/i }));
    fireEvent.click(screen.getByRole("button", { name: /documentdb/i }));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("fires onChange with the engine id when an enabled engine is clicked", () => {
    const onChange = vi.fn();
    // Start on a different value so clicking PostgreSQL is a real selection.
    render(<EngineField value="aidb" onChange={onChange} />);
    fireEvent.click(screen.getByRole("button", { name: /postgresql/i }));
    expect(onChange).toHaveBeenCalledWith("postgres");
  });
});
