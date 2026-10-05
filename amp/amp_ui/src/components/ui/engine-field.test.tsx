import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import {
  EngineField,
  DB_ENGINES,
  engineExtensions,
} from "@/components/ui/engine-field";
import catalog from "@/config/extensions.json";

// An engine is a named extension bundle the controller knows about. The
// selectable set must therefore be exactly the generated catalog's engines —
// offering one the controller would reject is a 400 the user cannot act on —
// while genuinely unimplemented engines stay disabled roadmap tiles.
afterEach(cleanup);

describe("EngineField", () => {
  it("makes exactly the catalog's engines selectable", () => {
    render(<EngineField value="postgres" onChange={vi.fn()} />);
    for (const e of catalog.engines) {
      const btn = screen.getByRole("button", { name: new RegExp(e.label, "i") });
      expect((btn as HTMLButtonElement).disabled).toBe(false);
    }
  });

  it("keeps unimplemented engines disabled and labelled Coming soon", () => {
    const onChange = vi.fn();
    render(<EngineField value="postgres" onChange={onChange} />);

    const roadmap = DB_ENGINES.filter((e) => e.comingSoon);
    for (const e of roadmap) {
      const btn = screen.getByRole("button", { name: new RegExp(e.label, "i") });
      expect((btn as HTMLButtonElement).disabled).toBe(true);
      fireEvent.click(btn);
    }
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.queryAllByText(/coming soon/i)).toHaveLength(roadmap.length);
  });

  it("lists each engine once, so a roadmap tile cannot shadow a shipped engine", () => {
    const ids = DB_ENGINES.map((e) => e.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("fires onChange with the engine id when a supported engine is clicked", () => {
    const onChange = vi.fn();
    render(<EngineField value="postgres" onChange={onChange} />);
    fireEvent.click(screen.getByRole("button", { name: /ai database/i }));
    expect(onChange).toHaveBeenCalledWith("aidb");
  });

  it("shows the bundle an engine seeds", () => {
    render(<EngineField value="postgres" onChange={vi.fn()} />);
    // AI Database is the multi-extension bundle; its card names what it brings.
    expect(screen.getByText(/pgvector · age · pg_cron/)).toBeTruthy();
  });

  it("engineExtensions returns the catalog's bundle, and nothing for unknowns", () => {
    expect(engineExtensions("aidb")).toEqual(["pgvector", "age", "pg_cron"]);
    expect(engineExtensions("oracle")).toEqual(["hexabridge"]);
    expect(engineExtensions("sqlserver")).toEqual(["hexabridge_mssql"]);
    expect(engineExtensions("postgres")).toEqual([]);
    expect(engineExtensions("mysql")).toEqual([]);
  });
});
