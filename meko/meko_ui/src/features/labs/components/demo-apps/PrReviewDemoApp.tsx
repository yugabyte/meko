// The ADLC Review Agent Lab's MIDDLE-pane demo app: a trimmed, purpose-built
// chat orchestrator (NOT a parameterized WorkbenchLMChat — that component
// bakes in WorkbenchLM-specific empty-state copy, a resume-thread dropdown,
// free-tier telemetry, and KB-readiness gating that don't apply to a single
// guided lab run). Reuses the generic leaf pieces that ARE shared: ChatInput,
// MarkdownMessage, the Message type, and the same postChat pipeline
// WorkbenchLMChat uses.

import { useCallback, useState } from "react";
import { AlertCircle, ExternalLink, Loader2, Send } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { MarkdownMessage } from "@/components/markdown-message";
import { getInferenceGatewayErrorMessage, postChat } from "@/lib/inferenceGateway";
import { ChatInput } from "@/features/workbench-lm/components/workbench-lm-chat/ChatInput";
import type { Message } from "@/features/workbench-lm/components/workbench-lm-chat/types";

import { fetchPullRequestDiff, fetchPullRequestSummary, parsePrRef } from "../../lib/githubApi";
import { postToSlackChannel } from "../../lib/slackApi";
import { SAMPLE_DIFF_CONTENT, SAMPLE_DIFF_TITLE } from "../../data/adlcReviewAgentLab/sampleDiff";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { PrDiffViewer } from "./PrDiffViewer";

function buildReviewPrompt(title: string, diff: string): string {
  return `Review this pull request:\n\nTitle: ${title}\n\n\`\`\`diff\n${diff}\n\`\`\``;
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
  const [sessionId] = useState(() => crypto.randomUUID());
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [sendError, setSendError] = useState<string | null>(null);
  // id of the first, diff-embedded user turn — hidden from the transcript
  // since PrDiffViewer already shows the diff; every later message (the
  // agent's reply, and any follow-up question/answer) renders normally.
  const [reviewPromptMessageId, setReviewPromptMessageId] = useState<string | null>(null);

  const [githubToken, setGithubToken] = useState("");
  const [prRefInput, setPrRefInput] = useState("");
  const [prError, setPrError] = useState<string | null>(null);
  const [fetchingPr, setFetchingPr] = useState(false);

  const [slackToken, setSlackToken] = useState("");
  const [slackChannel, setSlackChannel] = useState("");
  const [slackStatus, setSlackStatus] = useState<"idle" | "sending" | "sent" | "error">("idle");

  const canReview = Boolean(session.datapackId && session.userToken && session.userId);

  const send = useCallback(
    async (userText: string, isReviewPrompt = false) => {
      if (!session.datapackId || !session.userToken || !session.userId) return;
      setSending(true);
      setSendError(null);
      const userMessage: Message = { id: crypto.randomUUID(), role: "user", content: userText };
      if (isReviewPrompt) setReviewPromptMessageId(userMessage.id);
      setMessages((prev) => [...prev, userMessage]);
      try {
        const res = await postChat({
          message: userText,
          user_token: session.userToken,
          user_id: session.userId,
          session_id: sessionId,
          datapack_id: session.datapackId,
          conversation_id: conversationId,
          persona: demoApp.persona,
        });
        setConversationId(res.conversation_id);
        const assistantMessage: Message = {
          id: crypto.randomUUID(),
          role: "assistant",
          content: res.message,
          langfuseMessageId: res.message_id,
        };
        setMessages((prev) => [...prev, assistantMessage]);
        session.bumpRefreshTick();
      } catch (err) {
        setSendError(getInferenceGatewayErrorMessage(err));
      } finally {
        setSending(false);
      }
    },
    [session, sessionId, conversationId, demoApp.persona],
  );

  const handleRunReview = useCallback(async () => {
    setPrError(null);
    let title = SAMPLE_DIFF_TITLE;
    let diff = SAMPLE_DIFF_CONTENT;

    if (githubToken.trim() && prRefInput.trim()) {
      const ref = parsePrRef(prRefInput);
      if (!ref) {
        setPrError('Expected the format "owner/repo#123".');
        return;
      }
      setFetchingPr(true);
      try {
        const [summary, realDiff] = await Promise.all([
          fetchPullRequestSummary(ref, githubToken),
          fetchPullRequestDiff(ref, githubToken),
        ]);
        title = summary.title;
        diff = realDiff;
      } catch (err) {
        setPrError(err instanceof Error ? err.message : "Failed to fetch the pull request.");
        setFetchingPr(false);
        return;
      }
      setFetchingPr(false);
    }

    setDiffTitle(title);
    setDiffContent(diff);
    await send(buildReviewPrompt(title, diff), true);
  }, [githubToken, prRefInput, send]);

  const handlePostToSlack = useCallback(async () => {
    const lastAssistant = [...messages].reverse().find((m) => m.role === "assistant");
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

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex-1 overflow-y-auto p-4">
        {!hasReviewed ? (
          <div className="flex flex-col gap-4 rounded-md border border-border p-4">
            <div>
              <Label htmlFor="lab-gh-token">GitHub personal access token (optional)</Label>
              <Input
                id="lab-gh-token"
                type="password"
                placeholder="ghp_..."
                value={githubToken}
                onChange={(e) => setGithubToken(e.target.value)}
              />
            </div>
            <div>
              <Label htmlFor="lab-pr-ref">Pull request (owner/repo#123)</Label>
              <Input
                id="lab-pr-ref"
                placeholder="owner/repo#123"
                value={prRefInput}
                onChange={(e) => setPrRefInput(e.target.value)}
              />
            </div>
            {prError && (
              <p className="flex items-center gap-1.5 text-sm text-destructive">
                <AlertCircle className="h-4 w-4 shrink-0" /> {prError}
              </p>
            )}
            <p className="text-sm text-muted-foreground">
              Leave both blank to review the bundled sample diff instead.
            </p>
            <Button onClick={handleRunReview} disabled={!canReview || fetchingPr || sending}>
              {fetchingPr || sending ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Send className="h-4 w-4" />
              )}
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
                    <MarkdownMessage text={message.content} />
                  ) : (
                    message.content
                  )}
                </div>
              ))}
            {sending && <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />}
            {sendError && <p className="text-sm text-destructive">{sendError}</p>}

            {messages.some((m) => m.role === "assistant") && (
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
            void send(text);
          }}
          disabled={!canReview}
          sending={sending}
          blockedReason={canReview ? null : "Setting up this lab's datapack..."}
        />
      )}
    </div>
  );
}
