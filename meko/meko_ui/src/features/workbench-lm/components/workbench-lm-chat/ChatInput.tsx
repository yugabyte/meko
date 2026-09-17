// Bottom input bar for the WorkbenchLM chat. Textarea auto-grows with the
// content up to MAX_TEXTAREA_HEIGHT_PX (then scrolls). Enter submits,
// Shift+Enter inserts a newline. The Send button shows a spinner while a
// turn is in flight, and a sub-line under the bar surfaces the active
// blocker (no datapack, no token, knowledge not ready, etc.) so the user
// knows why typing is disabled.
//
// On sm+ (>=640px) the bar is styled as a Claude-Desktop-style compose
// surface: three visible rows at rest, a soft shadow that lifts on focus,
// and an inset from the viewport bottom edge — a single-row bar hugging
// the chrome was easy to overlook on tall screens. Below sm the bar stays
// as fa62153 shipped it (one-row rest, tighter padding) so the input keeps
// most of a 375 px viewport once the keyboard is up.

import type { KeyboardEvent } from "react";
import { useEffect, useLayoutEffect, useRef } from "react";
import { Loader2, Send } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Textarea } from "@/components/ui/textarea";

// ~10 lines of body text — past this the textarea scrolls instead of
// pushing the chat further off-screen.
const MAX_TEXTAREA_HEIGHT_PX = 240;
// Rest-height floors. The auto-grow effect writes an inline pixel
// height every render, which beats any `min-h-*` class — so gating
// the floor has to happen inside the effect, not in Tailwind. Mobile
// stays a one-row compact bar to preserve fa62153's 375 px viewport
// trim; sm+ gets a three-row compose surface for the "prompt is easy
// to lose on tall screens" fix.
const MIN_TEXTAREA_HEIGHT_BASE_PX = 36; // ~2.25rem
const MIN_TEXTAREA_HEIGHT_SM_PX = 72; // ~4.5rem
const SM_BREAKPOINT_QUERY = "(min-width: 640px)";

export function ChatInput({
  value,
  onChange,
  onSubmit,
  disabled,
  sending,
  blockedReason,
  compact = false,
}: {
  value: string;
  onChange: (next: string) => void;
  onSubmit: () => void;
  disabled: boolean;
  sending: boolean;
  blockedReason: string | null;
  /** Sits the composer on the page's own 14px body scale on DESKTOP instead
   *  of the 16px pin below. Mobile keeps 16px either way. For a chat embedded
   *  in a step alongside ordinary prose, where a larger composer reads as a
   *  different app. */
  compact?: boolean;
}) {
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  // Clamp between the breakpoint-aware rest-height floor and the
  // MAX_TEXTAREA_HEIGHT_PX ceiling, and toggle the scrollbar only when
  // real content overflows the ceiling. `window.matchMedia` is guarded
  // for test environments (jsdom does not define it) — falling back to
  // the mobile floor is the safe default, keeping the effect side-effect-
  // free in tests instead of throwing.
  const applyAutoGrow = () => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    const isSmUp =
      typeof window !== "undefined" &&
      typeof window.matchMedia === "function" &&
      window.matchMedia(SM_BREAKPOINT_QUERY).matches;
    const minHeight = isSmUp ? MIN_TEXTAREA_HEIGHT_SM_PX : MIN_TEXTAREA_HEIGHT_BASE_PX;
    const next = Math.min(Math.max(el.scrollHeight, minHeight), MAX_TEXTAREA_HEIGHT_PX);
    el.style.height = `${next}px`;
    el.style.overflowY = el.scrollHeight > MAX_TEXTAREA_HEIGHT_PX ? "auto" : "hidden";
  };

  // Resize on every value change. useLayoutEffect avoids a one-frame flicker
  // where the old height paints before the new one is measured.
  useLayoutEffect(() => {
    applyAutoGrow();
  }, [value]);

  // Re-measure on window resize — wrapping width changes the line count,
  // and crossing the sm breakpoint changes the floor.
  useEffect(() => {
    window.addEventListener("resize", applyAutoGrow);
    return () => window.removeEventListener("resize", applyAutoGrow);
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
      // Mobile: sticky-bottom + safe-area-inset (safe area preserved on
      // every viewport — the max(env, …) floor stays in the >=sm rule
      // too, so landscape iPhone / iPad respect the home-indicator gap).
      // Tighter horizontal padding below sm so the input field gets
      // most of a 375px viewport, and the prominence bump (roomier
      // card, 3-row rest height, larger text) only fires at >=sm so
      // mobile keeps the compact bar that fa62153 established.
      className="sticky bottom-0 z-10 border-t border-border bg-background px-3 pt-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] sm:px-6 sm:pt-5 sm:pb-[max(1.25rem,env(safe-area-inset-bottom))]"
    >
      <div className="mx-auto w-full max-w-3xl">
        <div className="flex items-end gap-2 rounded-xl border border-border bg-card p-2 shadow-sm transition-all focus-within:border-brand/60 focus-within:shadow-md sm:rounded-2xl sm:p-3">
          <Textarea
            ref={textareaRef}
            value={value}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={handleKeyDown}
            placeholder="Ask anything about your knowledge…"
            disabled={disabled}
            // rows={1} lets the effect fully own the rest height. A
            // literal rows={3} would put ~3 line-boxes into scrollHeight
            // for an empty textarea, and the effect writes that back as
            // an inline pixel height that beats any min-h-* class — so
            // gating growth in Tailwind can't preserve a compact mobile
            // bar. See MIN_TEXTAREA_HEIGHT_*_PX above.
            rows={1}
            style={{ maxHeight: MAX_TEXTAREA_HEIGHT_PX }}
            // md:text-base pins the body at 16 px on desktop against
            // Textarea's own md:text-sm override (components/ui/textarea.tsx:12);
            // an unprefixed text-base would lose the twMerge conflict.
            // md:leading-relaxed pairs with it. Mobile is already
            // text-base / leading-normal from the primitive, so no
            // unprefixed override is needed.
            className={cn(
              "resize-none overflow-hidden border-0 bg-transparent p-2 placeholder:text-muted-foreground/80 focus-visible:ring-0 focus-visible:ring-offset-0",
              // md: only -- the primitive's mobile text-base is deliberate,
              // since iOS Safari zooms the viewport on a sub-16px input.
              compact ? "md:text-sm" : "md:text-base md:leading-relaxed",
            )}
          />
          <Button
            type="submit"
            size="icon"
            disabled={disabled || value.trim().length === 0}
            // Keep size="icon"'s h-11 w-11 mobile default (44 px WCAG
            // touch target); only override the md:h-9 md:w-9 desktop
            // shrink so the button lines up with the roomier compose
            // card at >=768px. Adding an unprefixed h-* here would
            // drop the mobile touch target below 44 px.
            className="shrink-0 md:h-10 md:w-10"
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
          <p className="mt-2 px-1 text-xs text-muted-foreground">{blockedReason}</p>
        )}
      </div>
    </form>
  );
}
