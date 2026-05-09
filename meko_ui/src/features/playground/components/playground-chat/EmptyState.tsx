// Empty state for the Playground chat: a "try asking…" pill and two
// clickable example prompts that immediately send.

import { Sparkles } from "lucide-react";

const EXAMPLE_PROMPTS = [
  "Recommend a beach vacation in Europe under $3k",
  "Compare Spain vs. UK for a 2-week trip",
];

export function EmptyState({
  disabled,
  onPick,
}: {
  disabled: boolean;
  onPick: (prompt: string) => void;
}) {
  return (
    <div className="mx-auto flex h-full w-full max-w-3xl flex-col items-center justify-center px-6 py-10">
      <div className="inline-flex items-center gap-2 rounded-full border border-blue-500/30 bg-blue-500/10 px-3 py-1 text-xs text-blue-200">
        <Sparkles className="h-3.5 w-3.5" aria-hidden />
        Try asking about your knowledge base
      </div>
      <div className="mt-4 grid w-full grid-cols-1 gap-2 sm:grid-cols-2">
        {EXAMPLE_PROMPTS.map((prompt) => (
          <button
            key={prompt}
            type="button"
            onClick={() => onPick(prompt)}
            disabled={disabled}
            className="rounded-xl border border-border bg-[#1C2333] px-4 py-3 text-left text-sm text-foreground transition-colors hover:border-blue-400/60 hover:bg-blue-500/5 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {prompt}
          </button>
        ))}
      </div>
    </div>
  );
}
