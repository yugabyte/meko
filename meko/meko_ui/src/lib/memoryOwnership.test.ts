import { describe, expect, it } from "vitest";

import { isOwnMemory } from "@/lib/memoryOwnership";
import type { Memory } from "@/lib/api/mekoApi";

const ME = "2a6624ef-26c1-410c-bff3-dc111f2e45a2";
const SOMEONE_ELSE = "cdfc5c73-30b4-43bf-9d25-49c66f50b3d1";

function memory(fields: Partial<Memory>): Memory {
  return { id: "m1", data: "Raj likes Yugabyte", ...fields };
}

describe("isOwnMemory", () => {
  it("trusts is_mine over the attribution", () => {
    // The case the flag exists for: a viewer's listing is blanked, so the
    // id comparison would say "not mine" about the caller's own memory.
    expect(isOwnMemory(memory({ is_mine: true, user_id: "" }), ME)).toBe(true);
    expect(isOwnMemory(memory({ is_mine: false, user_id: ME }), ME)).toBe(false);
  });

  it("falls back to the id comparison when the server sends no is_mine", () => {
    expect(isOwnMemory(memory({ user_id: ME }), ME)).toBe(true);
    expect(isOwnMemory(memory({ user_id: SOMEONE_ELSE }), ME)).toBe(false);
  });

  it("matches the same id in a different case", () => {
    // The server compares as uuids and so does the delete's SQL; a
    // case-sensitive fallback would hide delete on the caller's own rows.
    expect(isOwnMemory(memory({ user_id: ME.toUpperCase() }), ME)).toBe(true);
    expect(isOwnMemory(memory({ user_id: ME }), ME.toUpperCase())).toBe(true);
  });

  it("claims nothing when neither the flag nor the attribution is present", () => {
    // An older server plus a viewer: no signal either way, so no delete
    // affordance rather than one on every row.
    expect(isOwnMemory(memory({}), ME)).toBe(false);
    expect(isOwnMemory(memory({ user_id: "" }), ME)).toBe(false);
  });

  it("does not match rows to an unknown account", () => {
    expect(isOwnMemory(memory({ user_id: "" }), undefined)).toBe(false);
    expect(isOwnMemory(memory({ user_id: ME }), undefined)).toBe(false);
  });
});
