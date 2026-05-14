// Bottom input bar for the Playground chat. Textarea auto-grows with the
// content up to MAX_TEXTAREA_HEIGHT_PX (then scrolls). Enter submits,
// Shift+Enter inserts a newline. The Send button shows a spinner while a
// turn is in flight, and a sub-line under the bar surfaces the active
// blocker (no datapack, no token, knowledge not ready, etc.) so the user
// knows why typing is disabled.

import type { KeyboardEvent } from "react";
import { useEffect, useLayoutEffect, useRef } from "react";
import { Loader2, Send } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";

// ~10 lines of body text — past this the textarea scrolls instead of
// pushing the chat further off-screen.
const MAX_TEXTAREA_HEIGHT_PX = 240;

export function ChatInput({
  value,
  onChange,
  onSubmit,
  disabled,
  sending,
  blockedReason,
}: {
  value: string;
  onChange: (next: string) => void;
  onSubmit: () => void;
  disabled: boolean;
  sending: boolean;
  blockedReason: string | null;
}) {
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  // Resize on every value change. useLayoutEffect avoids a one-frame flicker
  // where the old height paints before the new one is measured.
  useLayoutEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    const next = Math.min(el.scrollHeight, MAX_TEXTAREA_HEIGHT_PX);
    el.style.height = `${next}px`;
    el.style.overflowY = el.scrollHeight > MAX_TEXTAREA_HEIGHT_PX ? "auto" : "hidden";
  }, [value]);

  // Re-measure on window resize — wrapping width changes the line count.
  useEffect(() => {
    const onResize = () => {
      const el = textareaRef.current;
      if (!el) return;
      el.style.height = "auto";
      const next = Math.min(el.scrollHeight, MAX_TEXTAREA_HEIGHT_PX);
      el.style.height = `${next}px`;
      el.style.overflowY = el.scrollHeight > MAX_TEXTAREA_HEIGHT_PX ? "auto" : "hidden";
    };
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);

  const handleKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      onSubmit();
    }
  };

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (disabled || value.trim().length === 0) return;
        onSubmit();
      }}
      className="border-t border-border bg-background px-6 py-3"
    >
      <div className="mx-auto w-full max-w-3xl">
        <div className="flex items-end gap-2 rounded-xl border border-border bg-[#1C2333] p-2 transition-colors focus-within:border-blue-500/60">
          <Textarea
            ref={textareaRef}
            value={value}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Ask anything about your knowledge…"
            disabled={disabled}
            rows={1}
            style={{ maxHeight: MAX_TEXTAREA_HEIGHT_PX }}
            className="min-h-[2.25rem] resize-none overflow-hidden border-0 bg-transparent p-2 text-sm focus-visible:ring-0 focus-visible:ring-offset-0"
          />
          <Button
            type="submit"
            size="icon"
            disabled={disabled || value.trim().length === 0}
            className="h-9 w-9 shrink-0 bg-blue-600 hover:bg-blue-700 text-white disabled:bg-blue-600/40"
            aria-label="Send"
          >
            {sending ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <Send className="h-4 w-4" aria-hidden />
            )}
          </Button>
        </div>
        {blockedReason && (
          <p className="mt-1.5 px-1 text-[11px] text-muted-foreground">{blockedReason}</p>
        )}
      </div>
    </form>
  );
}
