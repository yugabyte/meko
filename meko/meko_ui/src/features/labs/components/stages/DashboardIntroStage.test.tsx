import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { DashboardIntroStage } from "./DashboardIntroStage";

afterEach(cleanup);

describe("DashboardIntroStage", () => {
  it("explains why a dashboard needs real storage", () => {
    render(<DashboardIntroStage />);
    expect(screen.getByText("Why a dashboard needs real storage")).toBeTruthy();
    expect(screen.getByText("A dashboard needs structure")).toBeTruthy();
  });
});
