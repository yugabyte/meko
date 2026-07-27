import { describe, it, expect } from "vitest";
import { parseSingleInviteEmail } from "@/features/datapacks/components/datapack-share/shareInviteEmail";

describe("parseSingleInviteEmail", () => {
  it("accepts and trims a single valid address", () => {
    expect(parseSingleInviteEmail("  user@example.com ")).toEqual({
      ok: true,
      email: "user@example.com",
    });
  });

  it("rejects an empty input", () => {
    expect(parseSingleInviteEmail("   ")).toEqual({
      ok: false,
      message: "Enter an email address.",
    });
  });

  it("rejects comma / semicolon separated lists", () => {
    expect(parseSingleInviteEmail("a@x.com, b@x.com")).toEqual({
      ok: false,
      message: "Enter one email address at a time.",
    });
    expect(parseSingleInviteEmail("a@x.com; b@x.com").ok).toBe(false);
  });

  it("rejects an address with internal whitespace", () => {
    expect(parseSingleInviteEmail("user name@example.com")).toEqual({
      ok: false,
      message: "Enter a single email address with no spaces.",
    });
  });

  it("rejects a malformed address (missing @ or domain)", () => {
    expect(parseSingleInviteEmail("not-an-email").message).toBe("Enter a valid email address.");
    expect(parseSingleInviteEmail("a@b").ok).toBe(false);
  });
});
