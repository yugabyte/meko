import { describe, it, expect } from "vitest";
import { validateDatapackName } from "@/lib/datapackNameValidation";

describe("validateDatapackName", () => {
  it("rejects empty / whitespace-only names", () => {
    expect(validateDatapackName("")).toBe("Enter a datapack name.");
    expect(validateDatapackName("   ")).toBe("Enter a datapack name.");
  });

  it("rejects names shorter than 3 characters (after trim)", () => {
    expect(validateDatapackName("ab")).toBe("Must have at least 3 characters");
    expect(validateDatapackName("  a  ")).toBe("Must have at least 3 characters");
  });

  it("rejects names longer than 60 characters", () => {
    expect(validateDatapackName("a".repeat(61))).toBe("Must have at most 60 characters");
  });

  it("rejects names containing HTML tags", () => {
    expect(validateDatapackName("my <b>pack</b>")).toBe("Text cannot contain HTML tags");
    expect(validateDatapackName("<script>x</script>")).toBe("Text cannot contain HTML tags");
  });

  it("accepts a valid name (returns null)", () => {
    expect(validateDatapackName("my-datapack")).toBeNull();
    expect(validateDatapackName("a".repeat(60))).toBeNull();
    expect(validateDatapackName("abc")).toBeNull();
  });
});
