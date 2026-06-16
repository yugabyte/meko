// Send-feedback modal triggered from the Sidebar. POST /feedback.
//
// - Category is required (Bug / Confusion / Feature request / Other).
// - Body is required (validated non-empty after trim).
// - Datapack scope (id + name) is sent only when filed from a datapack page.
// - "Include this page context" toggles whether `user_path` is sent; the
//   friendly datapack label beside the checkbox is shown only on named datapack routes.
// - source is hard-coded to "web_app"; source_identifier is the signed-in
//   email when available (BE will also auto-fill from the account).

import { useEffect, useState } from "react";
import { AlertCircle, Loader2, MessageSquare } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Checkbox } from "@/components/ui/checkbox";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import {
  FEEDBACK_CATEGORIES,
  getApiErrorMessage,
  submitFeedback,
  type FeedbackCategory,
} from "@/lib/api/mekoApi";

const CATEGORY_LABEL: Record<FeedbackCategory, string> = {
  bug: "Bug",
  confusion: "Confusion",
  feature_request: "Feature",
  other: "Other",
};

const MAX_FEEDBACK_LENGTH = 2000;

export type FeedbackDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Authenticated account id — required by the API. Dialog disables submit until present. */
  userId: string | null;
  /** Signed-in email (sent as `source_identifier` so the triage queue knows who to follow up with). */
  email: string | null;
  /** Current pathname (e.g. "/datapacks/foo"). Sent as `user_path` only when the context checkbox is on. */
  currentPath: string;
  /** Active datapack scope, when the user is on a datapack page. */
  datapackId: string | null;
  datapackName: string | null;
};

export function FeedbackDialog({
  open,
  onOpenChange,
  userId,
  email,
  currentPath,
  datapackId,
  datapackName,
}: FeedbackDialogProps) {
  const [category, setCategory] = useState<FeedbackCategory>("bug");
  const [body, setBody] = useState("");
  const [includeContext, setIncludeContext] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Reset on each open so a previous draft doesn't linger.
  useEffect(() => {
    if (open) {
      setCategory("bug");
      setBody("");
      setIncludeContext(false);
      setSubmitting(false);
      setError(null);
    }
  }, [open]);

  const trimmedBody = body.trim();
  const bodyEmpty = trimmedBody.length === 0;
  const bodyTooLong = trimmedBody.length > MAX_FEEDBACK_LENGTH;
  const canSubmit = !!userId && !submitting && !bodyEmpty && !bodyTooLong;


  const handleSubmit = async () => {
    if (!userId || !canSubmit) return;
    setSubmitting(true);
    setError(null);
    try {
      await submitFeedback({
        user_id: userId,
        feedback_text: trimmedBody,
        category,
        source: "web_app",
        created_at: new Date().toISOString(),
        datapack_id: datapackId ?? null,
        datapack_name: datapackName ?? null,
        user_path: includeContext ? currentPath || null : null,
        source_identifier: email && email.trim() !== "" ? email : null,
      });
      toast({
        title: "Feedback sent",
        description: "Thanks — we have received your feedback and will review it.",
      });
      onOpenChange(false);
    } catch (err) {
      setError(getApiErrorMessage(err, "Could not send feedback. Please try again."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-xl bg-card border-border text-foreground p-0 gap-0">
        <DialogHeader className="px-6 pt-6 pb-3 border-b border-border">
          <DialogTitle className="flex items-center gap-2 text-xl text-foreground">
            <MessageSquare className="h-5 w-5 text-blue-400" aria-hidden />
            Send feedback
          </DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Spotted a bug or have an idea? Let us know — we read everything.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-5 px-6 py-5">
          {/* Category picker */}
          <div className="space-y-3">
            <label className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
              What kind of feedback?
            </label>
            <div className="flex flex-wrap gap-2">
              {FEEDBACK_CATEGORIES.map((cat) => {
                const active = category === cat;
                return (
                  <button
                    key={cat}
                    type="button"
                    onClick={() => setCategory(cat)}
                    disabled={submitting}
                    className={cn(
                      "rounded-full border px-3.5 py-1.5 text-sm font-medium transition-colors",
                      active
                        ? "border-blue-500/60 bg-blue-500/15 text-blue-700 dark:text-blue-100"
                        : "border-border bg-background/60 text-muted-foreground hover:border-border/80 hover:text-foreground",
                      submitting && "cursor-not-allowed opacity-50",
                    )}
                  >
                    {CATEGORY_LABEL[cat]}
                  </button>
                );
              })}
            </div>
          </div>

          {/* Body */}
          <div className="space-y-2">
            <label
              htmlFor="feedback-body"
              className="text-xs font-semibold uppercase tracking-wider text-muted-foreground"
            >
              Tell us what's on your mind
            </label>
            <Textarea
              id="feedback-body"
              value={body}
              onChange={(e) => setBody(e.target.value)}
              placeholder="What worked, what didn't, or what you'd love to see next…"
              disabled={submitting}
              maxLength={MAX_FEEDBACK_LENGTH + 200} // soft over-limit; we still show validation
              className="min-h-[120px] resize-y bg-background/60 text-sm"
            />
            <div className="text-[11px]">
              <span className={cn("text-muted-foreground", bodyTooLong && "text-red-400")}>
                {bodyTooLong
                  ? `${trimmedBody.length}/${MAX_FEEDBACK_LENGTH} — please shorten`
                  : `${trimmedBody.length}/${MAX_FEEDBACK_LENGTH}`}
              </span>
            </div>
          </div>

          {/* Compact opt-in: friendly page name, no raw URL. */}
          <label className="flex items-center gap-2 text-xs text-muted-foreground cursor-pointer">
            <Checkbox
              checked={includeContext}
              onCheckedChange={(v) => setIncludeContext(v === true)}
              disabled={submitting}
            />
            <span>
              I agree to share my session context
              {datapackName ? (
                <span className="text-foreground/80"> ({datapackName})</span>
              ) : null}
            </span>
          </label>

          {error ? (
            <div className="flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-700 dark:text-red-300">
              <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
              <span>{error}</span>
            </div>
          ) : null}

          {!userId ? (
            <p className="text-xs text-amber-700 dark:text-amber-300">
              You need to be signed in to send feedback.
            </p>
          ) : null}

          <div className="flex justify-end gap-2 pt-1">
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button
              type="button"
              onClick={() => void handleSubmit()}
              disabled={!canSubmit}
              className="gap-2 bg-blue-600 hover:bg-blue-700 text-white disabled:bg-blue-600/40"
            >
              {submitting ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> : null}
              {submitting ? "Sending…" : "Send feedback"}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}

