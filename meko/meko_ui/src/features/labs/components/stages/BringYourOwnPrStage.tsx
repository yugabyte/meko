// The bring-your-own-PR flow, hosted by MakeItYoursStage: lets the user point
// AgentK at a real pull request instead of the bundled sample. Titleless on
// purpose -- the hosting stage owns the heading. The PR is fetched entirely client-side (githubApi.ts,
// "Called directly from the browser with a user-pasted PAT" -- GitHub's REST
// API sets permissive CORS for PAT-authenticated requests) using the user's
// own GitHub personal access token, which is never sent to Meko's backend or
// the review agent: once fetched, the title/diff go through PrReviewDemoApp
// via the SAME pr_title/pr_diff payload fields the sample-diff run already
// uses, so this needs zero backend or agent changes.

import { useState } from "react";
import { AlertCircle, Github, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  GitHubApiError,
  fetchPullRequestDiff,
  fetchPullRequestSummary,
  parsePrRef,
} from "@/lib/api/githubApi";

import type { ReviewAgentConfig } from "../demo-apps/ReviewAgentConfigPanel";
import {
  PrReviewDemoApp,
  type PrReviewRunState,
  type PrSource,
} from "../demo-apps/PrReviewDemoApp";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageNeedsDeployment } from "../lab-runner/LabStageNeedsDeployment";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";

export function BringYourOwnPrStage({
  demoApp,
  session,
  reviewConfig,
  onGoToCreateDatapack,
  onGoToDeployAgent,
  savedRun,
  onRunChange,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  /** Chosen in Chapter 2's Configure step; threaded straight through. */
  reviewConfig: ReviewAgentConfig;
  onGoToCreateDatapack: () => void;
  onGoToDeployAgent: () => void;
  savedRun?: PrReviewRunState | null;
  onRunChange?: (run: PrReviewRunState) => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  if (!session.deploymentActive) {
    return (
      <LabStageNeedsDeployment
        onGoToDeployAgent={onGoToDeployAgent}
        deploymentFailed={session.deployment?.status === "failed"}
      />
    );
  }
  return (
    <BringYourOwnPrStageContent
      demoApp={demoApp}
      session={session}
      reviewConfig={reviewConfig}
      savedRun={savedRun}
      onRunChange={onRunChange}
    />
  );
}

function BringYourOwnPrStageContent({
  demoApp,
  session,
  reviewConfig,
  savedRun,
  onRunChange,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  reviewConfig: ReviewAgentConfig;
  savedRun?: PrReviewRunState | null;
  onRunChange?: (run: PrReviewRunState) => void;
}) {
  const [prRefInput, setPrRefInput] = useState("");
  const [token, setToken] = useState("");
  const [fetching, setFetching] = useState(false);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [prSource, setPrSource] = useState<PrSource | null>(null);

  // A review already ran (restored from savedRun, e.g. after navigating away
  // and back) -- render PrReviewDemoApp directly, same as RunReviewAgainStage.
  // handleRunReview won't fire again for this mount, so prSource being empty
  // here (this component's own local state resets on remount) doesn't matter.
  const alreadyReviewed = Boolean(savedRun?.diffContent);

  const handleFetch = async () => {
    const ref = parsePrRef(prRefInput);
    if (!ref) {
      setFetchError("Paste a GitHub PR link, or use the format owner/repo#123.");
      return;
    }
    const trimmedToken = token.trim();
    if (!trimmedToken) {
      setFetchError("Paste a GitHub personal access token.");
      return;
    }
    setFetching(true);
    setFetchError(null);
    try {
      const [summary, diff] = await Promise.all([
        fetchPullRequestSummary(ref, trimmedToken),
        fetchPullRequestDiff(ref, trimmedToken),
      ]);
      setPrSource({ title: summary.title, diff, htmlUrl: summary.htmlUrl || undefined });
    } catch (err) {
      setFetchError(
        err instanceof GitHubApiError ? err.message : "Could not fetch that pull request.",
      );
    } finally {
      setFetching(false);
    }
  };

  // Top-aligned and content-sized: this renders inside the hosting stage's
  // section, not as a full-height stage of its own, so `flex-1
  // justify-center` centred it in a fixed box and clipped both ends. The
  // heading and the "what this is" line belong to the host.
  if (!alreadyReviewed && !prSource) {
    return (
      <div className="flex w-full flex-col gap-3 rounded-lg border border-border p-4">
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Github className="h-4 w-4 shrink-0 text-primary" aria-hidden />
          Fetched in your browser with your own token, which never reaches Meko.
        </div>
        {/* Two short fields side by side at width: stacked full-bleed, each
            input runs far wider than anything anyone pastes into it. */}
        <div className="grid gap-3 sm:grid-cols-2">
          <div>
            <Label htmlFor="byoc-pr-ref">Pull request</Label>
            <Input
              id="byoc-pr-ref"
              autoFocus
              placeholder="https://github.com/owner/repo/pull/123"
              value={prRefInput}
              onChange={(e) => setPrRefInput(e.target.value)}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              Paste a PR link, or owner/repo#123.
            </p>
          </div>
          <div>
            <Label htmlFor="byoc-pr-token">GitHub personal access token</Label>
            <Input
              id="byoc-pr-token"
              type="password"
              placeholder="ghp_…"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void handleFetch();
              }}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              Needs read access to the repo: a fine-grained token scoped to "Pull requests:
              Read-only" is enough.
            </p>
          </div>
        </div>
        {fetchError && (
          <p className="flex items-center gap-1.5 text-sm text-destructive">
            <AlertCircle className="h-4 w-4 shrink-0" /> {fetchError}
          </p>
        )}
        <Button
          className="self-start"
          onClick={() => void handleFetch()}
          disabled={!prRefInput.trim() || !token.trim() || fetching}
        >
          {fetching && <Loader2 className="h-4 w-4 animate-spin" />}
          {fetching ? "Fetching…" : "Fetch PR"}
        </Button>
      </div>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <PrReviewDemoApp
        demoApp={demoApp}
        session={session}
        reviewConfig={reviewConfig}
        prSource={prSource ?? undefined}
        savedRun={savedRun}
        onRunChange={onRunChange}
      />
    </div>
  );
}
