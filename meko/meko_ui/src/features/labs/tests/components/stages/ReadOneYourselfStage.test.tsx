// This lesson's whole argument is that the loop closes in public: the agent
// makes a checkable claim and the engineer who got it answers. A rendering
// that dropped the replies would leave only the agent talking.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import {
  FEATURED_REVIEW,
  pullRequestUrl,
  reviewCommentUrl,
} from "@/features/labs/data/adlcReviewAgentLab/publicReviews";
import { ReadOneYourselfStage } from "@/features/labs/components/stages/ReadOneYourselfStage";

afterEach(cleanup);

describe("ReadOneYourselfStage", () => {
  it("shows a reply under every comment", () => {
    render(<ReadOneYourselfStage />);
    expect(FEATURED_REVIEW.comments.length).toBeGreaterThan(0);
    for (const comment of FEATURED_REVIEW.comments) {
      expect(comment.reply.trim().length).toBeGreaterThan(0);
    }
    expect(screen.getAllByText(/replied$/)).toHaveLength(FEATURED_REVIEW.comments.length);
  });

  it("links each comment to its own anchor on GitHub", () => {
    render(<ReadOneYourselfStage />);
    const links = screen.getAllByRole("link", { name: /on GitHub/i }) as HTMLAnchorElement[];
    expect(links.map((a) => a.href)).toEqual(
      FEATURED_REVIEW.comments.map((c) => reviewCommentUrl(FEATURED_REVIEW.pr, c.id)),
    );
  });

  it("links the header to the pull request itself", () => {
    render(<ReadOneYourselfStage />);
    const link = screen.getByRole("link", {
      name: new RegExp(FEATURED_REVIEW.title.slice(0, 25), "i"),
    }) as HTMLAnchorElement;
    expect(link.href).toBe(pullRequestUrl(FEATURED_REVIEW.pr));
  });

  // The bodies are trimmed. Saying so is the difference between an excerpt
  // and a misquote.
  it("says the comments are trimmed", () => {
    render(<ReadOneYourselfStage />);
    expect(screen.getByText(/trimmed to the claim and its reasoning/i)).toBeTruthy();
  });
});
