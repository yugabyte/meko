import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { DashboardIntroStage } from "@/features/labs/components/stages/DashboardIntroStage";

afterEach(cleanup);

describe("DashboardIntroStage", () => {
  it("frames the dashboard as this datapack's own record", () => {
    render(<DashboardIntroStage />);
    expect(screen.getByText("Where a dashboard's data comes from")).toBeTruthy();
    expect(screen.getByText("Meko already knows your activity")).toBeTruthy();
    expect(screen.getByText("Your own runs, not a sample")).toBeTruthy();
  });

  it("admits which numbers don't persist, rather than letting the next screen imply they do", () => {
    render(<DashboardIntroStage />);
    expect(screen.getByText(/aren't persisted anywhere yet/)).toBeTruthy();
  });
});
