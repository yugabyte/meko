import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { GitHubApiError } from "@/lib/api/githubApi";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { BringYourOwnPrStage } from "./BringYourOwnPrStage";

// Deep coverage of the review flow itself lives in PrReviewDemoApp.test.tsx --
// this test only locks the fetch-form-vs-degrade-vs-review switch this stage
// owns, plus that a fetched PR is handed through as `prSource`.
const mockPrReviewDemoApp = vi.fn();
vi.mock("../demo-apps/PrReviewDemoApp", () => ({
  PrReviewDemoApp: (props: unknown) => {
    mockPrReviewDemoApp(props);
    return <div data-testid="pr-review-demo-app-mock" />;
  },
}));

vi.mock("@/lib/api/githubApi", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api/githubApi")>("@/lib/api/githubApi");
  return {
    ...actual,
    fetchPullRequestSummary: vi.fn(),
    fetchPullRequestDiff: vi.fn(),
  };
});

import { fetchPullRequestDiff, fetchPullRequestSummary } from "@/lib/api/githubApi";

const mockFetchSummary = vi.mocked(fetchPullRequestSummary);
const mockFetchDiff = vi.mocked(fetchPullRequestDiff);

const DEMO_APP: LabDemoAppConfig = { kind: "pr-review-chat", agentDisplayName: "AgentK" };

function fakeSession(datapackId: string | null, deploymentActive = false): ReturnType<typeof useLabRunnerSession> {
  return {
    datapackId,
    deploymentActive,
    deployment: null,
  } as unknown as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);

describe("BringYourOwnPrStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession(null)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  it("shows the deployment-needed prompt when a datapack is set but no deployment is active", () => {
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", false)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByText(/deploy your agent first/i)).toBeTruthy();
  });

  it("shows the fetch form, not PrReviewDemoApp, before a PR has been fetched", () => {
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    expect(screen.getByPlaceholderText("https://github.com/owner/repo/pull/123")).toBeTruthy();
    expect(screen.queryByTestId("pr-review-demo-app-mock")).toBeNull();
  });

  it("rejects a malformed PR reference before ever calling the GitHub client", async () => {
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText("https://github.com/owner/repo/pull/123"), { target: { value: "not-a-valid-ref" } });
    fireEvent.change(screen.getByPlaceholderText("ghp_…"), { target: { value: "tok" } });
    fireEvent.click(screen.getByRole("button", { name: /fetch pr/i }));

    await waitFor(() => expect(screen.getByText(/paste a github pr link/i)).toBeTruthy());
    expect(mockFetchSummary).not.toHaveBeenCalled();
    expect(mockFetchDiff).not.toHaveBeenCalled();
  });

  it("shows the GitHub client's error message when the fetch fails", async () => {
    mockFetchSummary.mockRejectedValue(new GitHubApiError("GitHub API request failed: 404 Not Found"));
    mockFetchDiff.mockRejectedValue(new GitHubApiError("GitHub API request failed: 404 Not Found"));
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText("https://github.com/owner/repo/pull/123"), { target: { value: "acme/widgets#42" } });
    fireEvent.change(screen.getByPlaceholderText("ghp_…"), { target: { value: "tok" } });
    fireEvent.click(screen.getByRole("button", { name: /fetch pr/i }));

    await waitFor(() => expect(screen.getByText(/404 Not Found/)).toBeTruthy());
    expect(screen.queryByTestId("pr-review-demo-app-mock")).toBeNull();
  });

  it("fetches the PR then renders PrReviewDemoApp with it as prSource", async () => {
    mockFetchSummary.mockResolvedValue({ title: "Fix the thing", htmlUrl: "https://github.com/acme/widgets/pull/42" });
    mockFetchDiff.mockResolvedValue("diff --git a/x b/x\n+fixed\n");
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText("https://github.com/owner/repo/pull/123"), { target: { value: "acme/widgets#42" } });
    fireEvent.change(screen.getByPlaceholderText("ghp_…"), { target: { value: "tok" } });
    fireEvent.click(screen.getByRole("button", { name: /fetch pr/i }));

    await waitFor(() => expect(screen.getByTestId("pr-review-demo-app-mock")).toBeTruthy());
    const props = mockPrReviewDemoApp.mock.calls.at(-1)?.[0];
    expect(props.prSource).toEqual({
      title: "Fix the thing",
      diff: "diff --git a/x b/x\n+fixed\n",
      htmlUrl: "https://github.com/acme/widgets/pull/42",
    });
  });

  it("renders PrReviewDemoApp directly, skipping the fetch form, when a review was already restored from savedRun", () => {
    const savedRun = { diffContent: "diff --git a/x b/x\n" } as never;
    render(
      <BringYourOwnPrStage
        demoApp={DEMO_APP}
        session={fakeSession("dp-1", true)}
        onGoToCreateDatapack={vi.fn()}
        onGoToDeployAgent={vi.fn()}
        savedRun={savedRun}
      />,
    );
    expect(screen.getByTestId("pr-review-demo-app-mock")).toBeTruthy();
    expect(screen.queryByPlaceholderText("https://github.com/owner/repo/pull/123")).toBeNull();
  });
});
