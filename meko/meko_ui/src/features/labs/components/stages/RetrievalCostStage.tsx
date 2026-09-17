// Chapter 2's cost lesson: the two ways to get a standards document into a
// model, side by side. A teaching comparison, not a setting — AgentK always
// retrieves, so picking a card explains the trade-off rather than changing
// anything.

import { useState } from "react";

import { WarningBanner } from "@/components/ui/warning-banner";

import { ORG_CODING_STANDARDS_CONTENT } from "../../data/adlcReviewAgentLab/orgCodingStandards";
import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import {
  costMultiple,
  estimatePasteTokens,
  estimatePasteTokensFromDocuments,
} from "../../lib/retrievalCostEstimate";
import { LabChoiceCard } from "../LabChoiceCard";
import { LabStageHeader } from "./LabStageHeader";

type RetrievalMode = "paste" | "query";

/** Retrieval pulls back the rules that match one diff, not the document. A
 *  quarter of it is the working figure this lesson quotes; the measured
 *  number lands in Chapter 3. */
const RETRIEVED_FRACTION = 4;

/** Pre-selected because it's what the agent actually does: opening on the
 *  retrieval card states the real behaviour, and the user clicks the other
 *  one to see what it would have cost instead. */
const DEFAULT_MODE: RetrievalMode = "query";

export function RetrievalCostStage({
  agentDisplayName,
  feed,
}: {
  agentDisplayName: string;
  /** The datapack's own knowledge documents, so this quotes the size of what
   *  the user actually uploaded rather than the bundled sample. */
  feed: LabContextFeed;
}) {
  const [mode, setMode] = useState<RetrievalMode>(DEFAULT_MODE);

  // The user's own documents when there are any, the bundled sample when the
  // lesson is reached before anything was uploaded.
  const uploadedTokens = estimatePasteTokensFromDocuments(feed.knowledge, feed.chunkSize);
  const fromUpload = uploadedTokens !== null;
  const pasteTokens = uploadedTokens ?? estimatePasteTokens(ORG_CODING_STANDARDS_CONTENT);
  const queryTokens = Math.max(Math.round(pasteTokens / RETRIEVED_FRACTION), 1);
  const multiple = costMultiple(pasteTokens, queryTokens);

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 4"
        title="How should it read the standards?"
        lede="Same document, two ways to get it into the model. This choice is the whole cost argument, so make it yourself."
        tags={["Cost"]}
      />

      <div className="grid gap-3 sm:grid-cols-2">
        <LabChoiceCard
          title="Paste the whole doc"
          body="Stuff the entire standards document into the prompt on every single review. Simple, and what most teams do today."
          metric={`~${pasteTokens.toLocaleString()} input tokens / review`}
          selected={mode === "paste"}
          onSelect={() => setMode("paste")}
        />
        <LabChoiceCard
          title="Ask for the rules that apply"
          body={`${agentDisplayName} searches the datapack for rules relevant to this diff and pulls back only those, plus your memories.`}
          metric={`~${queryTokens.toLocaleString()} input tokens / review`}
          selected={mode === "query"}
          onSelect={() => setMode("query")}
        />
      </div>

      {/* The consequence of the choice, in the app's own warning/success
          colours -- picking the expensive option should read as a warning,
          not as neutral prose the user can skim past. */}
      {mode === "paste" && multiple !== null && (
        <WarningBanner tone="warning">
          <strong className="font-semibold">{multiple}× the input, same answer.</strong> You re-send it for
          every agent and every retry, and the model still has to find the one rule that matters.
        </WarningBanner>
      )}
      {mode === "query" && (
        <WarningBanner tone="success">
          <strong className="font-semibold">
            ~{(pasteTokens - queryTokens).toLocaleString()} fewer input tokens per review.
          </strong>{" "}
          The document is embedded once for the whole org; each agent reads only what its task needs.
          Nobody re-indexes anything.
        </WarningBanner>
      )}

      <p className="text-xs text-muted-foreground">
        {fromUpload
          ? `Both figures are estimates from the size of what you uploaded (${feed.knowledgeTotal} document${feed.knowledgeTotal === 1 ? "" : "s"}), shown before anything has run.`
          : "Both figures are estimates from the sample document's size, shown before anything has run."}
        {` ${agentDisplayName} always retrieves, so there's no paste mode to switch on. `}
        Your review's real measured cost appears in Chapter 3.
      </p>
    </div>
  );
}
