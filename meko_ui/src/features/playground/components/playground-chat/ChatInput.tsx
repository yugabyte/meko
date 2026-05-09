// Bottom input bar for the Playground chat. Textarea grows to a max of
// ~6 lines, Enter submits, Shift+Enter inserts a newline. The Send button
// shows a spinner while a turn is in flight, and a sub-line under the bar
// surfaces the active blocker (no datapack, no token, knowledge not ready,
// etc.) so the user knows why typing is disabled.

import type { KeyboardEvent } from "react";
import { Loader2, Send } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";

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
            value={value}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Ask anything about your knowledge…"
            disabled={disabled}
            rows={1}
            className="min-h-[2.25rem] max-h-32 resize-none border-0 bg-transparent p-2 text-sm focus-visible:ring-0 focus-visible:ring-offset-0"
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
