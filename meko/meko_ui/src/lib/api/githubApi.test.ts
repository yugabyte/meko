import { describe, expect, it } from "vitest";

import { parsePrRef } from "./githubApi";

describe("parsePrRef", () => {
  it("parses the short owner/repo#123 form", () => {
    expect(parsePrRef("acme/widgets#42")).toEqual({ owner: "acme", repo: "widgets", number: 42 });
  });

  it("parses a pasted GitHub PR URL", () => {
    expect(parsePrRef("https://github.com/ameyb/global-tariff-genai-rag-app/pull/1")).toEqual({
      owner: "ameyb",
      repo: "global-tariff-genai-rag-app",
      number: 1,
    });
  });

  it("parses a PR URL with a trailing path, query, or fragment", () => {
    expect(parsePrRef("https://github.com/acme/widgets/pull/42/files")).toEqual({
      owner: "acme",
      repo: "widgets",
      number: 42,
    });
    expect(parsePrRef("https://github.com/acme/widgets/pull/42?diff=unified")).toEqual({
      owner: "acme",
      repo: "widgets",
      number: 42,
    });
    expect(parsePrRef("https://github.com/acme/widgets/pull/42#issuecomment-1")).toEqual({
      owner: "acme",
      repo: "widgets",
      number: 42,
    });
  });

  it("tolerates surrounding whitespace", () => {
    expect(parsePrRef("  acme/widgets#42  ")).toEqual({
      owner: "acme",
      repo: "widgets",
      number: 42,
    });
  });

  it("returns null for anything else", () => {
    expect(parsePrRef("not-a-valid-ref")).toBeNull();
    expect(parsePrRef("https://gitlab.com/acme/widgets/pull/1")).toBeNull();
    expect(parsePrRef("")).toBeNull();
  });
});
