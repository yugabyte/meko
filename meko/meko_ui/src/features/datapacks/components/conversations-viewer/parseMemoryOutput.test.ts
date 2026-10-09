import { describe, it, expect } from "vitest";
import { parseMemoryOutput } from "@/features/datapacks/components/conversations-viewer/parseMemoryOutput";

describe("parseMemoryOutput", () => {
  it("returns [] for empty / null-ish / empty-container inputs", () => {
    for (const raw of ["", "   ", "null", '""', "{}"]) {
      expect(parseMemoryOutput(raw)).toEqual([]);
    }
  });

  it("returns the raw string when it isn't JSON", () => {
    expect(parseMemoryOutput("just some text")).toEqual(["just some text"]);
  });

  it("unboxes a JSON-encoded (and double-encoded) string", () => {
    expect(parseMemoryOutput('"hello"')).toEqual(["hello"]);
    expect(parseMemoryOutput('"\\"hello\\""')).toEqual(["hello"]);
  });

  it("extracts memories from a mem0 { results: [{ memory }] } shape", () => {
    const raw = JSON.stringify({
      results: [{ memory: "likes tea" }, { memory: " coffee " }, { memory: "" }],
    });
    expect(parseMemoryOutput(raw)).toEqual(["likes tea", "coffee"]);
  });

  it("extracts a single { memory } / { text } field", () => {
    expect(parseMemoryOutput(JSON.stringify({ memory: "one" }))).toEqual(["one"]);
    expect(parseMemoryOutput(JSON.stringify({ text: "two" }))).toEqual(["two"]);
  });

  it("falls back to stringifying an object with no known text field", () => {
    const raw = JSON.stringify({ foo: 1 });
    expect(parseMemoryOutput(raw)).toEqual([JSON.stringify({ foo: 1 })]);
  });
});
