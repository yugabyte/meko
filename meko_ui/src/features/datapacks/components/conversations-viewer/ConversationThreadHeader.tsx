// Top strip of the thread pane — conversation title, started/last-activity
// timestamps, per-conversation stat chips (messages, memories extracted,
// llm calls), and the Observe (open in Langfuse trace view) + Continue
// (placeholder) action buttons.
//
// Pulled out of `ConversationThreadPane` so the orchestrator file stays
// focused on data + layout while this component owns the presentation of
// the conversation's metadata.

import { Bot, Cpu, ExternalLink, MessageSquare, Send, User } from "lucide-react";
import { Button } from "@/components/ui/button";
import { MemoryIcon } from "@/components/conversation-telemetry";
import type { DatapackConversationDetail } from "@/lib/api/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import { ROUTES } from "@/app/routes/paths";
import { formatRelativeTime } from "@/lib/relativeTime";
import { Chip } from "./Chip";
import { IndexConversationButton } from "./IndexConversationButton";

type Props = {
  detail: DatapackConversationDetail;
  conversationId: string;
  langfuseProjectId: string;
  /** Author email, forwarded from the rail's list rows because the
   *  conversation-detail endpoint doesn't return `user_email_id`. The
   *  detail's own value is preferred when present (future-proofs against
   *  the backend later including it). */
  userEmail?: string;
  /** Datapack the conversation belongs to — needed to fetch the MCP token
   *  and index the conversation. */
  datapackId?: string;
  /** Called after a successful index so the parent refetches the detail. */
  onIndexed?: () => void;
  /** Current epoch ms — ticks via `useNowMs` in the parent so the
   *  "last activity" relative time stays fresh without a per-component
   *  timer. */
  now: number;
};

export function ConversationThreadHeader({
  detail,
  conversationId,
  langfuseProjectId,
  userEmail,
  datapackId,
  onIndexed,
  now,
}: Props) {
  const headline = detail.title || detail.first_message || "(no messages yet)";
  const startedDisplay = formatDatapackCreatedAtUtc(detail.started_at);
  const authorEmail = detail.user_email_id || userEmail;

  return (
    <header className="border-b border-border bg-card px-6 py-4 shrink-0">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-base font-medium text-foreground" title={headline}>
            {headline}
          </h2>
          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
            {detail.agent_id ? (
              <>
                <span className="inline-flex items-center gap-1">
                  <Bot className="h-3 w-3" aria-hidden />
                  <code className="text-blue-700 dark:text-blue-300">{detail.agent_id}</code>
                </span>
                <span>·</span>
              </>
            ) : null}
            <span title={detail.started_at}>started {startedDisplay}</span>
            <span>·</span>
            <span>
              last activity {formatRelativeTime(detail.last_activity_at, now)}
            </span>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-1.5">
            <Chip icon={<MessageSquare className="h-3 w-3" />} accent="slate">
              {(detail.messages ?? 0).toLocaleString()} messages
            </Chip>
            {(detail.memories_added ?? 0) > 0 ? (
              <Chip icon={<MemoryIcon className="text-[14px]" />} accent="blue">
                {detail.memories_added.toLocaleString()} memories extracted
              </Chip>
            ) : null}
            {(detail.llm_calls ?? 0) > 0 ? (
              <Chip icon={<Cpu className="h-3 w-3" />} accent="amber">
                {detail.llm_calls.toLocaleString()} llm calls
              </Chip>
            ) : null}
            {authorEmail ? (
              <Chip icon={<User className="h-3 w-3" />} accent="slate">
                {authorEmail}
              </Chip>
            ) : null}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {/*
            Observe link — opens the Observe Hub for this Langfuse session
            in the same tab. (The shared `<ObserveButton>` opens in a new
            tab; the conversation viewer's button has always been in-app,
            so we keep that behaviour distinct here.)
          */}
          {langfuseProjectId ? (
            <a
              href={`${ROUTES.observeHub}?${new URLSearchParams({ project: langfuseProjectId, session: conversationId }).toString()}`}
              className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/10 px-2 py-1 text-[11px] font-medium text-foreground/80 hover:bg-muted/20 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-purple-500/40 transition-colors"
              title="Open this conversation in the Langfuse trace view"
            >
              <ExternalLink className="h-3 w-3" aria-hidden />
              <span>Observe</span>
            </a>
          ) : null}
          {detail.can_index && datapackId ? (
            <IndexConversationButton
              datapackId={datapackId}
              conversationId={conversationId}
              onIndexed={onIndexed}
            />
          ) : null}
          <Button
            variant="outline"
            size="sm"
            className="gap-1.5 border-border bg-background text-muted-foreground hover:text-foreground"
            disabled
            title="Continuing this conversation from the viewer is not wired yet"
          >
            <Send className="h-3.5 w-3.5" aria-hidden />
            Continue
          </Button>
        </div>
      </div>
    </header>
  );
}
