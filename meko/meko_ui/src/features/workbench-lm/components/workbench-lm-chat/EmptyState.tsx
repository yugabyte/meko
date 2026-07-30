// Empty state for the WorkbenchLM chat: a "try asking…" pill and two
// clickable example prompts that immediately send.
//
// The two example prompts are tailored to the bundled sample knowledge file
// (`europe-travel-guide.md`). We only render them when that specific file is
// in the user's KB — otherwise the prompts would suggest content the LLM
// can't ground against. When the sample isn't loaded, we fall back to a
// shorter "describe your KB" hint that works for any uploaded file.

import { Sparkles } from "lucide-react";

const EXAMPLE_PROMPTS = [
  "Recommend a beach vacation in Europe under $3k",
  "Compare Spain vs. UK for a 2-week trip",
];

type Props = {
  disabled: boolean;
  onPick: (prompt: string) => void;
  /** True when the bundled `europe-travel-guide.md` sample is present in
   *  the active datapack's KB. Drives whether we show the travel-themed
   *  example prompts. */
  showSamplePrompts: boolean;
};

export function EmptyState({ disabled, onPick, showSamplePrompts }: Props) {
  return (
    <div className="mx-auto flex h-full w-full max-w-3xl flex-col items-center justify-center px-6 py-10">
      <div className="inline-flex items-center gap-2 rounded-full border border-brand/30 bg-brand/10 px-3 py-1 text-xs text-foreground">
        <Sparkles className="h-3.5 w-3.5 text-brand" aria-hidden />
        Try asking about your knowledge base
      </div>
      {showSamplePrompts ? (
        <div className="mt-4 grid w-full grid-cols-1 gap-2 sm:grid-cols-2">
          {EXAMPLE_PROMPTS.map((prompt) => (
            <button
              key={prompt}
              type="button"
              onClick={() => onPick(prompt)}
              disabled={disabled}
              className="rounded-xl border border-border bg-card px-4 py-3 text-left text-sm text-foreground transition-colors hover:border-brand/50 hover:bg-brand/5 disabled:cursor-not-allowed disabled:opacity-50"
            >
              {prompt}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
