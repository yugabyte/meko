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
import { InvokeDeploymentError, invokeDeployment, invokeSharedDeployment } from "@/lib/api/deploymentInvokeApi";
import { ChatInput } from "@/features/workbench-lm/components/workbench-lm-chat/ChatInput";
import type { Message } from "@/features/workbench-lm/components/workbench-lm-chat/types";

// Bring-your-own-PR (a real GitHub PAT + owner/repo#123) is deferred to a
// later chapter -- this stage only reviews the bundled sample diff for now.
// lib/api/githubApi.ts's fetchPullRequestDiff/fetchPullRequestSummary/
// parsePrRef stay in place unused here, for that future chapter to import.
import { postToSlackChannel } from "@/lib/api/slackApi";
import { SAMPLE_DIFF_CONTENT, SAMPLE_DIFF_TITLE } from "../../data/adlcReviewAgentLab/sampleDiff";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { PrDiffViewer } from "./PrDiffViewer";
import {
  DEFAULT_REVIEW_AGENT_CONFIG,
  ReviewAgentConfigPanel,
  type ReviewAgentConfig,
} from "./ReviewAgentConfigPanel";

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
  };
}

export function PrReviewDemoApp({
  demoApp,
  session,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  const [messages, setMessages] = useState<Message[]>([]);
  const [diffTitle, setDiffTitle] = useState<string | null>(null);
  const [diffContent, setDiffContent] = useState<string | null>(null);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [conversationId, setConversationId] = useState<string | null>(null);
  // The original review's full text, captured once and reused as the
  // context anchor for every follow-up question (not replaced by later
  // follow-up answers) -- matches agentcore_entrypoint.py's follow-up
  // payload contract (prior_review_text).
  const [reviewText, setReviewText] = useState<string | null>(null);
  const [sendError, setSendError] = useState<string | null>(null);
  // id of the first, diff-embedded user turn — hidden from the transcript
  // since PrDiffViewer already shows the diff. Every later message (the
  // agent's reply, and any follow-up question/answer) renders normally.
  const [reviewPromptMessageId, setReviewPromptMessageId] = useState<string | null>(null);

  const [slackToken, setSlackToken] = useState("");
  const [slackChannel, setSlackChannel] = useState("");
  const [slackStatus, setSlackStatus] = useState<"idle" | "sending" | "sent" | "error">("idle");

  const [reviewConfig, setReviewConfig] = useState<ReviewAgentConfig>(DEFAULT_REVIEW_AGENT_CONFIG);

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

  const send = useCallback(
    async (payload: Record<string, unknown>, userText: string, isReviewPrompt = false) => {
      if (session.deploymentMode === "dedicated" && !session.deploymentId) return;
      const controller = new AbortController();
      abortControllerRef.current = controller;
      setSending(true);
      setSendError(null);
      const userMessage: Message = { id: crypto.randomUUID(), role: "user", content: userText };
      if (isReviewPrompt) setReviewPromptMessageId(userMessage.id);
      const assistantId = crypto.randomUUID();
      setMessages((prev) => [...prev, userMessage, { id: assistantId, role: "assistant", content: "" }]);

      try {
        let fullReviewText = "";
        // Accumulated separately from the message content as a fallback: if
        // the stream ends without a `done` frame, fullReviewText would stay
        // "" (not null), wrongly flipping canFollowUp on with an empty
        // prior_review_text for every subsequent question.
        let accumulatedText = "";
        const stream =
          session.deploymentMode === "shared"
            ? invokeSharedDeployment(demoApp.deployable?.agentSlug ?? "", payload, controller.signal)
            : invokeDeployment(session.deploymentId as string, payload, controller.signal);
        for await (const event of stream) {
          if (event.type === "meta") {
            setConversationId(event.conversation_id);
          } else if (event.type === "text_delta") {
            accumulatedText += event.text;
            setMessages((prev) =>
              prev.map((m) => (m.id === assistantId ? { ...m, content: m.content + event.text } : m)),
            );
          } else if (event.type === "done") {
            fullReviewText = event.review;
            session.bumpRefreshTick();
          }
        }
        if (isReviewPrompt) setReviewText(fullReviewText || accumulatedText || null);
      } catch (err) {
        if (controller.signal.aborted) return; // unmounted mid-stream -- nothing left to show it to
        setSendError(err instanceof InvokeDeploymentError || err instanceof Error ? err.message : "Review failed.");
        // Drop the placeholder if nothing ever streamed into it -- otherwise
        // it lingers as a dead, permanently-empty card in the transcript.
        setMessages((prev) => prev.filter((m) => !(m.id === assistantId && m.content === "")));
      } finally {
        setSending(false);
      }
    },
    [session],
  );

  const handleRunReview = useCallback(async () => {
    const title = SAMPLE_DIFF_TITLE;
    const diff = SAMPLE_DIFF_CONTENT;

    setDiffTitle(title);
    setDiffContent(diff);
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
  }, [send, session.datapackId, session.userToken, session.deploymentMode, reviewConfig]);

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
    [conversationId, reviewText, send, session.datapackId, session.userToken, session.deploymentMode, reviewConfig],
  );

  const handlePostToSlack = useCallback(async () => {
    const lastAssistant = [...messages].reverse().find((m) => m.role === "assistant" && m.content.trim());
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

  const hasReviewed = diffContent !== null;
  const canFollowUp = canReview && conversationId !== null && reviewText !== null;

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex-1 overflow-y-auto p-4">
        {!hasReviewed ? (
          <div className="flex flex-col gap-4 rounded-md border border-border p-4">
            <ReviewAgentConfigPanel
              value={reviewConfig}
              onChange={setReviewConfig}
              agentDisplayName={demoApp.agentDisplayName}
            />
            <Button onClick={handleRunReview} disabled={!canReview || sending}>
              {sending ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
              Run review
            </Button>
          </div>
        ) : (
          <div className="flex flex-col gap-4">
            <PrDiffViewer title={diffTitle ?? SAMPLE_DIFF_TITLE} diff={diffContent} />
            {/* The diff-embedded review prompt itself is skipped — PrDiffViewer
                already shows the diff. Every later turn (reply + any
                follow-up) renders normally. */}
            {messages
              .filter((message) => message.id !== reviewPromptMessageId)
              .map((message) => (
                <div
                  key={message.id}
                  className={
                    message.role === "user"
                      ? "ml-auto max-w-[85%] rounded-md bg-primary/10 p-3 text-sm"
                      : "rounded-md border border-border bg-card p-3 text-sm"
                  }
                >
                  {message.role === "assistant" ? (
                    message.content ? (
                      <MarkdownMessage text={message.content} />
                    ) : (
                      <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
                    )
                  ) : (
                    message.content
                  )}
                </div>
              ))}
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
                    Failed to post — Slack's API may not allow this call directly from a browser
                    for this token type.
                  </p>
                )}
              </div>
            )}
          </div>
        )}
      </div>
      {hasReviewed && (
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
