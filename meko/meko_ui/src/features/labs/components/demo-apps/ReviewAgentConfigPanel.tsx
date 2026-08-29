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

import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";

export type ReviewTone = "concise" | "detailed" | "encouraging";
export type ReviewStrictness = "lenient" | "balanced" | "strict";

export type ReviewAgentConfig = {
  useKnowledgeBase: boolean;
  useMemories: boolean;
  focus: string;
  /** undefined = today's default phrasing, unchanged. */
  tone?: ReviewTone;
  /** undefined = today's default behavior, unchanged. */
  strictness?: ReviewStrictness;
};

export const DEFAULT_REVIEW_AGENT_CONFIG: ReviewAgentConfig = {
  useKnowledgeBase: true,
  useMemories: true,
  focus: "",
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

export function ReviewAgentConfigPanel({
  value,
  onChange,
  agentDisplayName,
}: {
  value: ReviewAgentConfig;
  onChange: (next: ReviewAgentConfig) => void;
  agentDisplayName: string;
}) {
  return (
    <div className="flex flex-col gap-4 rounded-md border border-border p-4">
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="review-agent-use-kb" className="flex flex-col gap-0.5">
          <span>Use org knowledge base</span>
          <span className="font-normal text-xs text-muted-foreground">
            Fetches your coding standards to ground this review.
          </span>
        </Label>
        <Switch
          id="review-agent-use-kb"
          checked={value.useKnowledgeBase}
          onCheckedChange={(checked) => onChange({ ...value, useKnowledgeBase: checked })}
        />
      </div>
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="review-agent-use-memories" className="flex flex-col gap-0.5">
          <span>Use my memories</span>
          <span className="font-normal text-xs text-muted-foreground">
            Pulls in anything Meko remembers from past reviews.
          </span>
        </Label>
        <Switch
          id="review-agent-use-memories"
          checked={value.useMemories}
          onCheckedChange={(checked) => onChange({ ...value, useMemories: checked })}
        />
      </div>
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
          Treated as advisory context, not new instructions — {agentDisplayName} still follows its review rules.
        </p>
      </div>
    </div>
  );
}
