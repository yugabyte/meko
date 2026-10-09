import { describe, it, expect } from "vitest";
import {
  dedent,
  beautifyStatement,
} from "@/features/datapacks/components/conversations-viewer/sqlBeautify";

describe("dedent", () => {
  it("strips the common leading indentation and trims", () => {
    expect(dedent("    a\n      b\n    c")).toBe("a\n  b\nc");
  });
  it("converts tabs to two spaces before measuring indent", () => {
    expect(dedent("\ta\n\t\tb")).toBe("a\n  b");
  });
});

describe("beautifyStatement", () => {
  it("breaks a single-line SELECT before each major clause and indents booleans", () => {
    expect(beautifyStatement("pg.query", "SELECT a FROM t WHERE x AND y")).toBe(
      "SELECT a\nFROM t\nWHERE x\n  AND y",
    );
  });

  it("uppercases mid-statement clause keywords (leading token left as typed unless a full clause phrase)", () => {
    // The leading-uppercase step captures two words ("select a"), which isn't a
    // recognized clause, so `select` stays as typed; `from` becomes `\nFROM`.
    expect(beautifyStatement("pg.query", "select a from t")).toBe("select a\nFROM t");
  });

  it("keeps a JOIN modifier on the JOIN line", () => {
    expect(beautifyStatement("pg.query", "SELECT a FROM t LEFT JOIN u ON u.id = t.id")).toContain(
      "\nLEFT JOIN u ON u.id = t.id",
    );
  });

  it("leaves Cypher statements alone (only dedented)", () => {
    const cypher = "MATCH (n) RETURN n";
    expect(beautifyStatement("neo4j.cypher.read", cypher)).toBe(cypher);
  });

  it("returns empty input unchanged", () => {
    expect(beautifyStatement("pg.query", "   ")).toBe("");
  });
});
