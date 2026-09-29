// "Configure your reviewer" — the Lab's guided agent-customization step.
// Pure local-state UI, modeled on DeployAgentPanel.tsx's "compose this
// thing" shape but simpler (no async calls here). Every field here is a
// REAL setting: PrReviewDemoApp forwards them (reviewConfigPayload) as
// enable_knowledge_base / enable_memory_search / focus / tone / strictness
// in the invokeDeployment body, which api_server's /deployments/:id/invoke
// passes verbatim to the deployed agent. There, ReviewConfig genuinely
// skips a disabled tool, wraps the focus text as an explicitly delimited
// advisory-only block, or (for tone/strictness) selects a canned
// instruction line — never a raw prompt override. See the adlc-review-agent
// source (yugabyte/canned-agents) for the exact rendering.

import { useState, type ReactNode } from "react";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

export type ReviewTone = "concise" | "detailed" | "encouraging";
export type ReviewStrictness = "lenient" | "balanced" | "strict";

// Small Bedrock models the user can pick to compare against the Claude
// reference review shown above this panel (see AnthropicReferenceCard) --
// real Bedrock model ids, forwarded verbatim as the invoke payload's `model`
// field, which agentcore_entrypoint.py's _resolve_bedrock_model already
// reads in preference to its own Claude default
// (https://github.com/yugabyte/canned-agents/pull/3). Deliberately no
// "Claude (default)" option here: this stage's whole point is comparing a
// small model's output against the fixed reference above, not re-running
// Claude live on every visit -- see the reference card for that comparison
// point instead.
export type SlmModelId =
  | "mistral.ministral-3-14b-instruct"
  | "us.meta.llama4-maverick-17b-instruct-v1:0"
  | "amazon.nova-lite-v1:0";

export type ReviewAgentConfig = {
  useKnowledgeBase: boolean;
  useMemories: boolean;
  focus: string;
  /** undefined = today's default phrasing, unchanged. */
  tone?: ReviewTone;
  /** undefined = today's default behavior, unchanged. */
  strictness?: ReviewStrictness;
  /** Always one of the three SLMs below -- unlike tone/strictness there is
   *  no "default" state, since the whole point of this step is comparing a
   *  deliberately-chosen smaller model against the Claude reference. */
  model: SlmModelId;
};

export const MODEL_OPTIONS: { value: SlmModelId; label: string }[] = [
  { value: "mistral.ministral-3-14b-instruct", label: "Ministral 14B" },
  { value: "us.meta.llama4-maverick-17b-instruct-v1:0", label: "Llama 4 Maverick 17B" },
  { value: "amazon.nova-lite-v1:0", label: "Nova Lite" },
];

export const MODEL_LABELS: Record<SlmModelId, string> = Object.fromEntries(
  MODEL_OPTIONS.map((opt) => [opt.value, opt.label]),
) as Record<SlmModelId, string>;

export const DEFAULT_REVIEW_AGENT_CONFIG: ReviewAgentConfig = {
  useKnowledgeBase: true,
  useMemories: true,
  focus: "",
  model: MODEL_OPTIONS[0].value,
};

const FOCUS_MAX_LENGTH = 300;

// Radix Select can't carry `undefined` as an item value, so "no selection"
// is its own sentinel here and mapped back to undefined on change.
const UNSET = "default";

const TONE_OPTIONS: { value: ReviewTone; label: string }[] = [
  { value: "concise", label: "Concise" },
  { value: "detailed", label: "Detailed" },
  { value: "encouraging", label: "Encouraging" },
];

const STRICTNESS_OPTIONS: { value: ReviewStrictness; label: string }[] = [
  { value: "lenient", label: "Lenient" },
  { value: "balanced", label: "Balanced" },
  { value: "strict", label: "Strict" },
];

// A compact pill toggle, in place of a full label+subtext+Switch row --
// labeled with the actual MCP tool name it gates (knowledgebase_search /
// memory_search), not a feature-shaped paraphrase, so the connection to
// "same tool calls" (see the paragraph below the Model select) is visible
// at a glance. Kept as a real `role="switch"` control, same semantics as
// the Switch component it replaces here.
function ToggleChip({
  id,
  checked,
  onCheckedChange,
  children,
}: {
  id: string;
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  children: ReactNode;
}) {
  return (
    <button
      id={id}
      type="button"
      role="switch"
      aria-checked={checked}
      onClick={() => onCheckedChange(!checked)}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors",
        checked
          ? "border-primary/40 bg-primary/10 text-foreground"
          : "border-border bg-muted/40 text-muted-foreground",
      )}
    >
      <span className={cn("h-1.5 w-1.5 flex-none rounded-full", checked ? "bg-primary" : "bg-muted-foreground/50")} />
      {children}
    </button>
  );
}

export function ReviewAgentConfigPanel({
  value,
  onChange,
  agentDisplayName,
  hideModel,
  hideTools,
}: {
  value: ReviewAgentConfig;
  onChange: (next: ReviewAgentConfig) => void;
  agentDisplayName: string;
  /** The caller already offers the model as its own control (the model cards
   *  in HowAgentReviewsStage), so this panel would be a second, disagreeing
   *  place to change it. */
  hideModel?: boolean;
  /** Same, for the two search toggles: where the caller frames them as a
   *  read-the-datapack-or-not choice, showing them again as raw tool
   *  switches lets the two drift apart. */
  hideTools?: boolean;
}) {
  const [advancedOpen, setAdvancedOpen] = useState(false);

  return (
    <div className="flex flex-col gap-3 rounded-md border border-border p-4">
      {!hideTools && (
      <div className="flex flex-wrap items-center gap-2">
        <ToggleChip
          id="review-agent-use-kb"
          checked={value.useKnowledgeBase}
          onCheckedChange={(checked) => onChange({ ...value, useKnowledgeBase: checked })}
        >
          <code className="font-mono">knowledgebase_search</code>
        </ToggleChip>
        <ToggleChip
          id="review-agent-use-memories"
          checked={value.useMemories}
          onCheckedChange={(checked) => onChange({ ...value, useMemories: checked })}
        >
          <code className="font-mono">memory_search</code>
        </ToggleChip>
      </div>
      )}

      {!hideModel && (
      <div>
        <Label htmlFor="review-agent-model">Model</Label>
        <Select
          value={value.model}
          onValueChange={(v) => onChange({ ...value, model: v as SlmModelId })}
        >
          <SelectTrigger id="review-agent-model">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {MODEL_OPTIONS.map((opt) => (
              <SelectItem key={opt.value} value={opt.value}>
                {opt.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="mt-1 text-xs text-muted-foreground">
          Reviewed with a smaller model, grounded in the same context, to compare against
          Claude's reference review above.
        </p>
      </div>
      )}

      <Collapsible open={advancedOpen} onOpenChange={setAdvancedOpen}>
        <CollapsibleTrigger className="text-left text-xs text-muted-foreground underline decoration-border underline-offset-2 hover:text-foreground">
          {advancedOpen ? "− Tone, strictness, focus" : "+ Tone, strictness, focus"}
        </CollapsibleTrigger>
        <CollapsibleContent className="mt-3 flex flex-col gap-3">
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label htmlFor="review-agent-tone">Tone</Label>
              <Select
                value={value.tone ?? UNSET}
                onValueChange={(v) => onChange({ ...value, tone: v === UNSET ? undefined : (v as ReviewTone) })}
              >
                <SelectTrigger id="review-agent-tone">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={UNSET}>Default</SelectItem>
                  {TONE_OPTIONS.map((opt) => (
                    <SelectItem key={opt.value} value={opt.value}>
                      {opt.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div>
              <Label htmlFor="review-agent-strictness">Strictness</Label>
              <Select
                value={value.strictness ?? UNSET}
                onValueChange={(v) =>
                  onChange({ ...value, strictness: v === UNSET ? undefined : (v as ReviewStrictness) })
                }
              >
                <SelectTrigger id="review-agent-strictness">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={UNSET}>Default</SelectItem>
                  {STRICTNESS_OPTIONS.map((opt) => (
                    <SelectItem key={opt.value} value={opt.value}>
                      {opt.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div>
            <div className="flex items-center justify-between">
              <Label htmlFor="review-agent-focus">Focus (optional)</Label>
              <span className="text-xs text-muted-foreground">
                {value.focus.length}/{FOCUS_MAX_LENGTH}
              </span>
            </div>
            <Textarea
              id="review-agent-focus"
              placeholder="e.g. pay extra attention to SQL injection risks"
              value={value.focus}
              maxLength={FOCUS_MAX_LENGTH}
              onChange={(e) => onChange({ ...value, focus: e.target.value })}
            />
            <p className="mt-1 text-xs text-muted-foreground">
              Treated as advisory context, not new instructions. {agentDisplayName} still follows its
              review rules.
            </p>
          </div>
        </CollapsibleContent>
      </Collapsible>
    </div>
  );
}
