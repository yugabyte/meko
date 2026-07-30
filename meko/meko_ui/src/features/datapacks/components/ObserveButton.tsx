/**
 * Shared "Observe" button that deep-links into the Langfuse trace viewer.
 * Accepts a pre-normalised `sessionId` (no dashes) and the Langfuse project ID.
 * Renders nothing when either value is absent.
 *
 * Always opens in a new tab — the Observe Hub is a side-trip from whatever
 * the user was looking at (conversations, memories, WorkbenchLM chat) and
 * jumping in-place would lose their scroll position + chat state. `noopener`
 * keeps the new tab from holding a reference back to our window object.
 */
import { ExternalLink } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ROUTES } from "@/app/routes/paths";

type Props = {
  /** Langfuse project ID (e.g. from listDatapackConversations). */
  projectId: string;
  /** Langfuse session / trace ID — must already have dashes stripped. */
  sessionId: string;
  /** Optional click handler — useful when the button lives inside another
   *  clickable element and needs e.stopPropagation(). */
  onClick?: (e: React.MouseEvent) => void;
};

export function ObserveButton({ projectId, sessionId, onClick }: Props) {
  if (!projectId || !sessionId) return null;

  const href = `${ROUTES.observeHub}?${new URLSearchParams({ project: projectId, session: sessionId }).toString()}`;

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="gap-1.5 border-border bg-background/80 text-blue-700 dark:text-blue-300 hover:bg-blue-500/10 hover:text-blue-700 dark:text-blue-200 hover:border-blue-500/40"
          asChild
        >
          {/*
            Plain <a> rather than react-router's <Link> — we want a real
            browser tab, not an in-app navigation. Same-origin link so the
            Observe Hub still loads fast from cache.
          */}
          <a
            href={href}
            target="_blank"
            rel="noreferrer noopener"
            onClick={onClick}
          >
            <ExternalLink className="h-3.5 w-3.5 shrink-0" aria-hidden />
            Observe
          </a>
        </Button>
      </TooltipTrigger>
      <TooltipContent side="top">Open in Langfuse trace viewer (new tab)</TooltipContent>
    </Tooltip>
  );
}
