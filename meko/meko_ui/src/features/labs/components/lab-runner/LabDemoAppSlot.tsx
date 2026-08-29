import { PrReviewDemoApp } from "../demo-apps/PrReviewDemoApp";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";

/**
 * MIDDLE pane of the lab runner: switches on demoApp.kind. Adding a second
 * lab with a new demo-app kind means adding a case here + a new component
 * under components/demo-apps/ — LabRunnerPage itself doesn't change.
 */
export function LabDemoAppSlot({
  demoApp,
  session,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  switch (demoApp.kind) {
    case "pr-review-chat":
      return <PrReviewDemoApp demoApp={demoApp} session={session} />;
    default:
      return null;
  }
}
