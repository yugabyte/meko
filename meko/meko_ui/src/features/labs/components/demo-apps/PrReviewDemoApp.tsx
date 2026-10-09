// The ADLC Review Agent Lab's MIDDLE-pane demo app: a trimmed, purpose-built
// chat orchestrator (NOT a parameterized WorkbenchLMChat — that component
// bakes in WorkbenchLM-specific empty-state copy, a resume-thread dropdown,
// free-tier telemetry, and KB-readiness gating that don't apply to a single
// guided lab run). Reuses the generic leaf pieces that ARE shared: ChatInput,
// MarkdownMessage, and the Message type.
//
// Reviews stream from the deployed agent (invokeDeployment), not a direct
// inference_gateway call — RunReviewStage already gates this component
// behind an active deployment, so there's no fallback path here. Two
// payload shapes go through the same streaming consumer: the initial
// review (pr_title/pr_diff + review config) and a follow-up question
// (conversation_id + prior_review_text, reusing the same conversation).

import { useCallback, useEffect, useRef, useState } from "react";
import { ExternalLink, Loader2, Send } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { MarkdownMessage } from "@/components/markdown-message";
import {
  InvokeDeploymentError,
  invokeDeployment,
  invokeSharedDeployment,
} from "@/lib/api/deploymentInvokeApi";
import { ChatInput } from "@/features/workbench-lm/components/workbench-lm-chat/ChatInput";
import type { Message } from "@/features/workbench-lm/components/workbench-lm-chat/types";

// Bring-your-own-PR: BringYourOwnPrStage fetches a real PR client-side (via
// lib/api/githubApi.ts, using the user's own pasted GitHub PAT -- which never
// reaches this component or Meko's backend) and hands the result in as
// `prSource`, which overrides the bundled sample diff below.
import { postToSlackChannel } from "@/lib/api/slackApi";
import type { ReviewStreamEvent } from "@/lib/api/deploymentInvokeApi";
import type { ReviewFinding } from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { SAMPLE_DIFF_CONTENT, SAMPLE_DIFF_TITLE } from "../../data/adlcReviewAgentLab/sampleDiff";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { recoverReviewFindings } from "../../lib/recoverReviewFindings";
import type { LabDemoAppConfig } from "../../types";
import { FindingsList } from "./FindingsList";
import { PrDiffViewer } from "./PrDiffViewer";
import { ReviewRunSummary } from "./ReviewRunSummary";
import { MODEL_LABELS, type ReviewAgentConfig, type SlmModelId } from "./ReviewAgentConfigPanel";

export type ContextStats = NonNullable<
  Extract<ReviewStreamEvent, { type: "done" }>["context_stats"]
>;

/** A real PR fetched client-side (BringYourOwnPrStage), overriding the
 *  bundled sample diff for this run. `htmlUrl` is optional purely because
 *  fetchPullRequestSummary's own response can come back without one. */
export type PrSource = { title: string; diff: string; htmlUrl?: string };

/**
 * Everything needed to fully restore a completed (or in-progress) review
 * after this component unmounts and remounts -- which happens on every
 * stage navigation, like any other stage component (see LabStageSlot's
 * switch). LabStageSlot owns the actual state (it's the ancestor that stays
 * mounted for the whole lab run) and passes it back in as `savedRun`;
 * PrReviewDemoApp just seeds its local state from it once on mount and
 * reports changes back via `onRunChange`. Deliberately narrower than every
 * piece of local state here -- `input` (an unsent follow-up draft) and the
 * Slack form fields are left to reset, since losing a draft or a pasted bot
 * token on navigation is a minor inconvenience, not "the results are gone."
 */
export type PrReviewRunState = {
  messages: Message[];
  diffTitle: string | null;
  diffContent: string | null;
  /** Whether THIS run's diff is the bundled sample, captured once at run
   *  time -- see isSample's own state comment for why this can't be derived
   *  from the `prSource` prop after a remount. */
  isSample: boolean;
  /** The real PR's GitHub URL for this run, if any -- same capture-once
   *  reasoning as isSample. */
  sourceUrl: string | null;
  conversationId: string | null;
  reviewText: string | null;
  reviewUsage: { input_tokens: number; output_tokens: number } | null;
  contextStats: ContextStats | null;
  reviewFindings: ReviewFinding[] | "unstructured" | null;
  initialReviewMessageId: string | null;
  selectedModelId: SlmModelId | null;
  reviewPromptMessageId: string | null;
  sendError: string | null;
  /** Wall clock for the review itself, measured here rather than reported
   *  by the agent: it is what the user sat through, round trip included. */
  durationMs: number | null;
};

function buildReviewPrompt(title: string, diff: string): string {
  return `Review this pull request:\n\nTitle: ${title}\n\n\`\`\`diff\n${diff}\n\`\`\``;
}

function reviewConfigPayload(config: ReviewAgentConfig) {
  return {
    enable_knowledge_base: config.useKnowledgeBase,
    enable_memory_search: config.useMemories,
    focus: config.focus.trim() || undefined,
    tone: config.tone,
    strictness: config.strictness,
    model: config.model,
  };
}

export function PrReviewDemoApp({
  demoApp,
  session,
  reviewConfig,
  prSource,
  savedRun,
  onRunChange,
  resultView = "chat",
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  /** Chosen in Chapter 2's Configure step and owned by LabStageSlot, so it
   *  survives this component remounting on every stage navigation. */
  reviewConfig: ReviewAgentConfig;
  /** A real PR to review instead of the bundled sample -- see PrSource's own
   *  comment. Only read at the moment "Run review" is clicked (handleRunReview);
   *  once a review is underway or restored from `savedRun`, this is ignored. */
  prSource?: PrSource;
  /** A previous run's results, if this component is remounting after the
   *  user navigated away and back -- see PrReviewRunState's own comment.
   *  Only read at the moment "Run review" is clicked (handleRunReview),
   *  which captures its title/diff/htmlUrl into diffTitle/diffContent/
   *  isSample/sourceUrl -- the post-review view reads THOSE, never
   *  `prSource` directly, since `prSource` is this component's OWN local
   *  state in BringYourOwnPrStage and resets to undefined on every remount,
   *  while a completed run's `savedRun` (and the state seeded from it)
   *  survives one. Reading `prSource` in the post-review render was a real
   *  bug (a restored real-PR run rendered mislabeled "Sample PR" with no
   *  GitHub link) -- fixed by capturing isSample/sourceUrl once instead. */
  savedRun?: PrReviewRunState | null;
  /** Called whenever any of PrReviewRunState's fields change, so the owner
   *  (LabStageSlot) can hand the same state back in as `savedRun` next time
   *  this component mounts. */
  onRunChange?: (run: PrReviewRunState) => void;
  /** "chat" is the full orchestrator: the findings inline, a Slack hand-off
   *  and a follow-up box. "summary" keeps the run button and reports what
   *  the run did, leaving the findings to the lesson that reads them, and
   *  is what Chapter 3's first lesson uses. */
  resultView?: "chat" | "summary";
}) {
  const [messages, setMessages] = useState<Message[]>(() => savedRun?.messages ?? []);
  const [diffTitle, setDiffTitle] = useState<string | null>(() => savedRun?.diffTitle ?? null);
  const [diffContent, setDiffContent] = useState<string | null>(
    () => savedRun?.diffContent ?? null,
  );
  // Captured once at send time (handleRunReview), same lifetime/reasoning as
  // selectedModelId below -- `prSource` itself is BringYourOwnPrStage's own
  // local state, which resets to undefined on every remount (stage
  // navigation), so the post-review view can't read it directly without
  // mislabeling a restored real-PR run as the bundled sample.
  const [isSample, setIsSample] = useState<boolean>(() => savedRun?.isSample ?? true);
  const [sourceUrl, setSourceUrl] = useState<string | null>(() => savedRun?.sourceUrl ?? null);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [conversationId, setConversationId] = useState<string | null>(
    () => savedRun?.conversationId ?? null,
  );
  // The original review's full text, captured once and reused as the
  // context anchor for every follow-up question (not replaced by later
  // follow-up answers) -- matches agentcore_entrypoint.py's follow-up
  // payload contract (prior_review_text).
  const [reviewText, setReviewText] = useState<string | null>(() => savedRun?.reviewText ?? null);
  // Captured once from the initial review's `done` event, same lifetime as
  // reviewText -- a follow-up turn is a separate LLM call with its own
  // usage, not part of the Claude-vs-SLM cost comparison this stage shows.
  const [reviewUsage, setReviewUsage] = useState<{
    input_tokens: number;
    output_tokens: number;
  } | null>(() => savedRun?.reviewUsage ?? null);
  // Real knowledgebase_search/memory_search retrieval counts (+ totals, from
  // datapack_describe) and an estimated per-tool token share for THIS run --
  // same lifetime as reviewUsage, see review.py's _build_context_stats.
  const [contextStats, setContextStats] = useState<ContextStats | null>(
    () => savedRun?.contextStats ?? null,
  );
  // Structured findings (severity/location/comment) parsed server-side from
  // the initial review's response -- see review.py's REVIEW_SYSTEM_PROMPT.
  // Three states, not two: `null` = still in flight; `"unstructured"` = done,
  // but the deployed image predates structured findings (the "findings" key
  // is absent from its "done" event entirely, not just empty) -- falls back
  // to the accumulated text_delta prose already in message.content, same as
  // before this feature shipped; an array = done, real structured findings.
  // A follow-up answer is never structured this way (see ReviewStreamEvent's
  // own doc comment), so this stays scoped to the one message identified by
  // initialReviewMessageId below.
  //
  // An EMPTY array is deliberately rendered the same way as "unstructured"
  // (see rendersAsFindings below), not as a confident "No issues found." --
  // review.py's _parse_findings degrades an unparseable response to
  // `findings=[]` too, with the model's actual raw text still in `review`.
  // Trusting an empty array at face value would silently hide a malformed
  // response behind a false "no issues" claim; falling back to message.content
  // shows the real text either way (which reads "No issues found." verbatim
  // when that's genuinely what review.py rendered for a real empty result).
  const [reviewFindings, setReviewFindings] = useState<ReviewFinding[] | "unstructured" | null>(
    () => savedRun?.reviewFindings ?? null,
  );
  // The initial review's own assistant bubble id -- rendered as FindingsList
  // once reviewFindings arrives, unlike every later (follow-up) assistant
  // bubble, which stays free-text prose through MarkdownMessage.
  const [initialReviewMessageId, setInitialReviewMessageId] = useState<string | null>(
    () => savedRun?.initialReviewMessageId ?? null,
  );
  // Captured at send time rather than read from reviewConfig.model later: the
  // user can go back to Chapter 2 and pick a different model after a review
  // has run, and the cost comparison must keep naming the model that run
  // actually used.
  const [selectedModelId, setSelectedModelId] = useState<SlmModelId | null>(
    () => savedRun?.selectedModelId ?? null,
  );
  const [sendError, setSendError] = useState<string | null>(() => savedRun?.sendError ?? null);
  const [durationMs, setDurationMs] = useState<number | null>(() => savedRun?.durationMs ?? null);
  // id of the first, diff-embedded user turn — hidden from the transcript
  // since PrDiffViewer already shows the diff. Every later message (the
  // agent's reply, and any follow-up question/answer) renders normally.
  const [reviewPromptMessageId, setReviewPromptMessageId] = useState<string | null>(
    () => savedRun?.reviewPromptMessageId ?? null,
  );

  const [slackToken, setSlackToken] = useState("");
  const [slackChannel, setSlackChannel] = useState("");
  const [slackStatus, setSlackStatus] = useState<"idle" | "sending" | "sent" | "error">("idle");

  // Shared mode needs no deploymentId/userToken of its own -- the shared
  // instance authenticates with its own credential, and RunReviewStage
  // already gates rendering this component behind session.deploymentActive.
  const canReview = Boolean(
    session.datapackId &&
    session.userId &&
    (session.deploymentMode === "shared" || Boolean(session.userToken && session.deploymentId)),
  );

  // Aborts an in-flight invokeDeployment call on unmount -- otherwise a user
  // navigating away mid-review leaves the fetch (and the AgentCore run it
  // proxies to) running, and any later stream event would set state on an
  // unmounted component.
  const abortControllerRef = useRef<AbortController | null>(null);
  useEffect(() => {
    return () => abortControllerRef.current?.abort();
  }, []);

  // Reports the current results up to LabStageSlot on every change, so
  // navigating away and back restores this exact view instead of starting
  // over -- see PrReviewRunState/savedRun's own comments. `sending` isn't
  // part of this: an in-flight request is aborted on unmount above, so
  // there's nothing left to resume, and restoring `sending: true` would
  // just show a permanently stuck spinner.
  useEffect(() => {
    onRunChange?.({
      messages,
      diffTitle,
      diffContent,
      isSample,
      sourceUrl,
      conversationId,
      reviewText,
      reviewUsage,
      contextStats,
      reviewFindings,
      initialReviewMessageId,
      selectedModelId,
      reviewPromptMessageId,
      sendError,
      durationMs,
    });
  }, [
    messages,
    diffTitle,
    diffContent,
    isSample,
    sourceUrl,
    conversationId,
    reviewText,
    reviewUsage,
    contextStats,
    reviewFindings,
    initialReviewMessageId,
    selectedModelId,
    reviewPromptMessageId,
    sendError,
    durationMs,
    onRunChange,
  ]);

  const send = useCallback(
    async (payload: Record<string, unknown>, userText: string, isReviewPrompt = false) => {
      if (session.deploymentMode === "dedicated" && !session.deploymentId) return;
      const controller = new AbortController();
      abortControllerRef.current = controller;
      const startedAt = performance.now();
      setSending(true);
      setSendError(null);
      if (isReviewPrompt) setDurationMs(null);
      const userMessage: Message = { id: crypto.randomUUID(), role: "user", content: userText };
      if (isReviewPrompt) setReviewPromptMessageId(userMessage.id);
      const assistantId = crypto.randomUUID();
      if (isReviewPrompt) setInitialReviewMessageId(assistantId);
      setMessages((prev) => [
        ...prev,
        userMessage,
        { id: assistantId, role: "assistant", content: "" },
      ]);

      try {
        let fullReviewText = "";
        // Accumulated separately from the message content as a fallback: if
        // the stream ends without a `done` frame, fullReviewText would stay
        // "" (not null), wrongly flipping canFollowUp on with an empty
        // prior_review_text for every subsequent question.
        let accumulatedText = "";
        const stream =
          session.deploymentMode === "shared"
            ? invokeSharedDeployment(
                demoApp.deployable?.agentSlug ?? "",
                payload,
                controller.signal,
              )
            : invokeDeployment(session.deploymentId as string, payload, controller.signal);
        for await (const event of stream) {
          if (event.type === "meta") {
            setConversationId(event.conversation_id);
          } else if (event.type === "text_delta") {
            accumulatedText += event.text;
            setMessages((prev) =>
              prev.map((m) =>
                m.id === assistantId ? { ...m, content: m.content + event.text } : m,
              ),
            );
          } else if (event.type === "done") {
            fullReviewText = event.review;
            // The initial review's response is structured findings JSON --
            // review.py never streams it as text_delta chunks (that would
            // show raw JSON mid-response), so this bubble's content is only
            // ever set here, once, not built up incrementally like a
            // follow-up's. Setting it for every "done" (not just the
            // findings case) keeps Slack-posting/canFollowUp's own read of
            // message.content correct either way.
            setMessages((prev) =>
              prev.map((m) => (m.id === assistantId ? { ...m, content: event.review } : m)),
            );
            // "findings" in event distinguishes "the key is genuinely absent"
            // (an old deployed image) from "present", whatever its value --
            // event.findings ?? null would collapse both to the same state
            // and get stuck showing a loading spinner forever against an old
            // image. A PRESENT empty array is not a "no issues" result; see
            // countableFindings.
            if (isReviewPrompt) {
              const reported = "findings" in event ? (event.findings ?? []) : null;
              // An empty list is the agent saying either "clean diff" or "I
              // could not read my own response" -- it sends [] for both. When
              // the raw text it also sends parses into findings, it was the
              // second. See recoverReviewFindings.
              const recovered =
                reported && reported.length === 0 ? recoverReviewFindings(event.review) : null;
              setReviewFindings(recovered ?? reported ?? "unstructured");
              // Set here, beside the findings, not only after the loop: an
              // error frame arriving after `done`, a dropped connection or an
              // unmount all skip the post-loop assignment, which would leave
              // the findings from this run next to the previous run's text --
              // and Chapter 3's step 2 renders that text when the findings
              // cannot be counted. The post-loop line stays for the case where
              // no `done` frame arrives at all.
              setReviewText(event.review);
            }
            if (isReviewPrompt && event.usage) setReviewUsage(event.usage);
            if (isReviewPrompt && event.context_stats) setContextStats(event.context_stats);
            session.bumpRefreshTick();
          }
        }
        if (isReviewPrompt) setReviewText(fullReviewText || accumulatedText || null);
      } catch (err) {
        if (controller.signal.aborted) return; // unmounted mid-stream -- nothing left to show it to
        setSendError(
          err instanceof InvokeDeploymentError || err instanceof Error
            ? err.message
            : "Review failed.",
        );
        // Drop the placeholder if nothing ever streamed into it -- otherwise
        // it lingers as a dead, permanently-empty card in the transcript.
        setMessages((prev) => prev.filter((m) => !(m.id === assistantId && m.content === "")));
      } finally {
        // Only the review itself is timed; a follow-up question is a
        // separate call and would overwrite the number the summary quotes.
        if (isReviewPrompt && !controller.signal.aborted) {
          setDurationMs(performance.now() - startedAt);
        }
        setSending(false);
      }
    },
    [session],
  );

  const handleRunReview = useCallback(async () => {
    const title = prSource?.title ?? SAMPLE_DIFF_TITLE;
    const diff = prSource?.diff ?? SAMPLE_DIFF_CONTENT;

    setDiffTitle(title);
    setDiffContent(diff);
    setIsSample(!prSource);
    setSourceUrl(prSource?.htmlUrl ?? null);
    setSelectedModelId(reviewConfig.model);
    await send(
      {
        datapack_id: session.datapackId,
        // Shared mode: the deployed instance authenticates with its own
        // credential, not one forwarded from the browser per request.
        ...(session.deploymentMode === "dedicated" ? { meko_pat: session.userToken } : {}),
        pr_title: title,
        pr_diff: diff,
        ...reviewConfigPayload(reviewConfig),
      },
      buildReviewPrompt(title, diff),
      true,
    );
  }, [send, session.datapackId, session.userToken, session.deploymentMode, reviewConfig, prSource]);

  const handleFollowUp = useCallback(
    (text: string) => {
      if (!conversationId || reviewText === null) return;
      void send(
        {
          datapack_id: session.datapackId,
          ...(session.deploymentMode === "dedicated" ? { meko_pat: session.userToken } : {}),
          conversation_id: conversationId,
          question: text,
          prior_review_text: reviewText,
          ...reviewConfigPayload(reviewConfig),
        },
        text,
      );
    },
    [
      conversationId,
      reviewText,
      send,
      session.datapackId,
      session.userToken,
      session.deploymentMode,
      reviewConfig,
    ],
  );

  const handlePostToSlack = useCallback(async () => {
    const lastAssistant = [...messages]
      .reverse()
      .find((m) => m.role === "assistant" && m.content.trim());
    if (!lastAssistant || !slackToken.trim() || !slackChannel.trim()) return;
    setSlackStatus("sending");
    try {
      await postToSlackChannel(
        slackToken.trim(),
        slackChannel.trim(),
        `*PR review* (${diffTitle ?? "sample diff"}):\n${lastAssistant.content}`,
      );
      setSlackStatus("sent");
    } catch {
      setSlackStatus("error");
    }
  }, [messages, slackToken, slackChannel, diffTitle]);

  // Named from the config rather than stated flatly: the Configure step can
  // switch either source off, and naming a tool the run was told not to call
  // is worse than saying less.
  const enabledTools = [
    reviewConfig.useKnowledgeBase && "knowledgebase_search",
    reviewConfig.useMemories && "memory_search",
  ].filter((t): t is string => typeof t === "string");

  const hasReviewed = diffContent !== null;
  const canFollowUp = canReview && conversationId !== null && reviewText !== null;
  const isSummary = resultView === "summary";
  // The `done` frame has landed, which is what turns the run button into its
  // finished state and releases the summary below it.
  const reviewComplete = hasReviewed && reviewFindings !== null && !sending;

  const configSentence = (
    <p className="text-sm text-muted-foreground">
      Reviewing with{" "}
      <span className="font-medium text-foreground">{MODEL_LABELS[reviewConfig.model]}</span>
      {enabledTools.length > 0 ? (
        <>
          , reading your datapack over{" "}
          {enabledTools.map((tool, i) => (
            <span key={tool}>
              {i > 0 && " and "}
              <code className="font-mono">{tool}</code>
            </span>
          ))}
          .
        </>
      ) : (
        // Both sources switched off in the Configure step: the run
        // reads nothing, which is a legitimate comparison to make
        // and should not be described as retrieval.
        ", with no datapack sources enabled."
      )}
    </p>
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex-1 overflow-y-auto p-4">
        {isSummary ? (
          // Scope tile, then the action, then what the run did. The findings
          // themselves belong to the next lesson.
          <div className="flex flex-col gap-6">
            <PrDiffViewer
              title={diffTitle ?? prSource?.title ?? SAMPLE_DIFF_TITLE}
              diff={diffContent ?? prSource?.diff ?? SAMPLE_DIFF_CONTENT}
              isSample={hasReviewed ? isSample : !prSource}
              url={(hasReviewed ? sourceUrl : prSource?.htmlUrl) ?? undefined}
              collapsible
            />
            <div className="flex flex-col gap-3 rounded-md border border-border p-4">
              {configSentence}
              <div className="flex flex-wrap items-center gap-3">
                <Button
                  busy={sending}
                  busyLabel="Reviewing"
                  done={reviewComplete}
                  doneLabel="Review complete"
                  disabled={!canReview}
                  onClick={handleRunReview}
                  className="self-start"
                >
                  <Send aria-hidden />
                  Run review
                </Button>
                {(sending || reviewComplete) && (
                  <span className="text-xs text-muted-foreground">
                    {demoApp.agentDisplayName}
                    {session.datapackName ? ` · reading ${session.datapackName}` : ""}
                  </span>
                )}
              </div>
            </div>
            {sendError && <p className="text-sm text-destructive">{sendError}</p>}
            {reviewComplete && reviewFindings !== null && (
              <ReviewRunSummary
                findings={reviewFindings}
                usage={reviewUsage}
                stats={contextStats}
                durationMs={durationMs}
                modelLabel={MODEL_LABELS[selectedModelId ?? reviewConfig.model]}
                enabledTools={enabledTools}
              />
            )}
          </div>
        ) : !hasReviewed ? (
          <div className="flex flex-col gap-4">
            {/* The bundled sample diff already gets its own preview in an
                earlier stage (review-sample-pr) before reaching this one --
                only a real, user-fetched PR (prSource) needs showing here,
                so "what's being reviewed" is visible in every phase: after
                fetching, while configuring, and (below, hasReviewed) during
                and after the review itself. */}
            {prSource && (
              <PrDiffViewer title={prSource.title} diff={prSource.diff} url={prSource.htmlUrl} />
            )}
            <div className="flex max-w-md flex-col gap-4 rounded-md border border-border p-4">
              {configSentence}
              <Button
                busy={sending}
                disabled={!canReview}
                onClick={handleRunReview}
                className="self-start"
              >
                <Send aria-hidden />
                Run review
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex flex-col gap-4">
            <PrDiffViewer
              title={diffTitle ?? SAMPLE_DIFF_TITLE}
              diff={diffContent}
              isSample={isSample}
              url={sourceUrl ?? undefined}
              collapsible
            />
            {/* The diff-embedded review prompt itself is skipped — PrDiffViewer
                already shows the diff. Every later turn (reply + any
                follow-up) renders normally. */}
            {messages
              .filter((message) => message.id !== reviewPromptMessageId)
              .map((message) => {
                const isInitialReview = message.id === initialReviewMessageId;
                const isStillReviewing = isInitialReview && reviewFindings === null;
                // Only a NON-empty findings array gets the FindingsList
                // treatment -- an empty array falls back to message.content
                // below, same as "unstructured", since review.py can't tell
                // "genuinely no issues" apart from "failed to parse" on the
                // wire (see reviewFindings' own doc comment above).
                const hasStructuredFindings =
                  isInitialReview && Array.isArray(reviewFindings) && reviewFindings.length > 0;
                const rendersAsFindings = isStillReviewing || hasStructuredFindings;
                return (
                  <div
                    key={message.id}
                    className={
                      message.role === "user"
                        ? "ml-auto max-w-[85%] rounded-md bg-primary/10 p-3 text-sm"
                        : rendersAsFindings
                          ? ""
                          : "rounded-md border border-border bg-card p-3 text-sm"
                    }
                  >
                    {message.role === "assistant" ? (
                      isStillReviewing ? (
                        <div className="flex items-center gap-2 rounded-md border border-border bg-card p-3 text-sm text-muted-foreground">
                          <Loader2 className="h-4 w-4 animate-spin" />
                          Reviewing…
                        </div>
                      ) : hasStructuredFindings ? (
                        <FindingsList findings={reviewFindings as ReviewFinding[]} />
                      ) : message.content ? (
                        <MarkdownMessage text={message.content} />
                      ) : (
                        <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
                      )
                    ) : (
                      message.content
                    )}
                  </div>
                );
              })}
            {sendError && <p className="text-sm text-destructive">{sendError}</p>}

            {messages.some((m) => m.role === "assistant" && m.content.trim()) && (
              <div className="flex flex-col gap-2 rounded-md border border-border p-3">
                <p className="text-sm font-medium">Post the review to Slack (optional)</p>
                <Input
                  placeholder="Slack bot token"
                  type="password"
                  value={slackToken}
                  onChange={(e) => setSlackToken(e.target.value)}
                />
                <Input
                  placeholder="Channel (e.g. #pr-reviews)"
                  value={slackChannel}
                  onChange={(e) => setSlackChannel(e.target.value)}
                />
                <Button
                  size="sm"
                  variant="outline"
                  onClick={handlePostToSlack}
                  disabled={!slackToken.trim() || !slackChannel.trim() || slackStatus === "sending"}
                >
                  {slackStatus === "sending" ? (
                    <Loader2 className="h-3.5 w-3.5 animate-spin" />
                  ) : (
                    <ExternalLink className="h-3.5 w-3.5" />
                  )}
                  Post to Slack
                </Button>
                {slackStatus === "sent" && (
                  <p className="text-sm text-emerald-600 dark:text-emerald-400">Posted.</p>
                )}
                {slackStatus === "error" && (
                  <p className="text-sm text-destructive">
                    Failed to post. Slack's API may not allow this call directly from a browser for
                    this token type.
                  </p>
                )}
              </div>
            )}
          </div>
        )}
      </div>
      {!isSummary && hasReviewed && (
        <ChatInput
          value={input}
          onChange={setInput}
          onSubmit={() => {
            const text = input.trim();
            if (!text) return;
            setInput("");
            handleFollowUp(text);
          }}
          disabled={!canFollowUp}
          sending={sending}
          blockedReason={canFollowUp ? null : "Waiting for the review to finish..."}
        />
      )}
    </div>
  );
}
