import { describe, expect, it } from "vitest";

import { regionFromRuntimeArn } from "./agentRuntimeArn";

describe("regionFromRuntimeArn", () => {
  it("reads the region out of a runtime ARN", () => {
    expect(regionFromRuntimeArn("arn:aws:bedrock-agentcore:us-west-2:1234:runtime/agentk")).toBe(
      "us-west-2",
    );
  });

  it("returns null for anything unparseable, so no region line is shown", () => {
    expect(regionFromRuntimeArn(undefined)).toBeNull();
    expect(regionFromRuntimeArn("")).toBeNull();
    expect(regionFromRuntimeArn("not-an-arn")).toBeNull();
    // A region-less ARN is well-formed but has nothing to report.
    expect(regionFromRuntimeArn("arn:aws:iam::1234:role/foo")).toBeNull();
  });
});
