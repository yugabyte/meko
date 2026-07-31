// ComputeSizeField — choose a compute reservation (cpu + memory) from named
// presets or Custom. Presentation-only; emits { cpu, memory }. Reusable across
// create / provision / resize flows. Theme-token styled (light/dark).

import { useState } from "react";
import { cn } from "@/lib/utils";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export type ComputeSize = { cpu: string; memory: string };
export type ComputeSizePreset = { id: string; label: string; cpu: string; memory: string };

export const DEFAULT_COMPUTE_PRESETS: ComputeSizePreset[] = [
  { id: "small", label: "Small", cpu: "2", memory: "4Gi" },
  { id: "medium", label: "Medium", cpu: "4", memory: "8Gi" },
  { id: "large", label: "Large", cpu: "8", memory: "16Gi" },
];

export function ComputeSizeField({
  value,
  onChange,
  presets = DEFAULT_COMPUTE_PRESETS,
  allowCustom = true,
  className,
}: {
  value: ComputeSize;
  onChange: (v: ComputeSize) => void;
  presets?: ComputeSizePreset[];
  allowCustom?: boolean;
  className?: string;
}) {
  // "Custom" is a mode, not derivable from value alone (a custom value can equal
  // a preset), so track it explicitly once the user opts in.
  const [customMode, setCustomMode] = useState(false);
  const matched = presets.find((p) => p.cpu === value.cpu && p.memory === value.memory);
  const isCustom = allowCustom && (customMode || !matched);

  const card = (on: boolean) =>
    cn(
      "rounded-lg border border-border bg-background p-3 text-left transition-colors hover:border-muted-foreground/40",
      on && "border-brand bg-brand/10 ring-1 ring-brand",
    );

  return (
    <div className={cn("space-y-3", className)}>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {presets.map((p) => {
          const on = !isCustom && matched?.id === p.id;
          return (
            <button
              key={p.id}
              type="button"
              aria-pressed={on}
              onClick={() => {
                setCustomMode(false);
                onChange({ cpu: p.cpu, memory: p.memory });
              }}
              className={card(on)}
            >
              <div className="text-sm font-semibold">{p.label}</div>
              <div className="font-mono text-xs text-muted-foreground">
                {p.cpu} vCPU · {p.memory}
              </div>
            </button>
          );
        })}
        {allowCustom && (
          <button
            type="button"
            aria-pressed={isCustom}
            onClick={() => setCustomMode(true)}
            className={card(isCustom)}
          >
            <div className="text-sm font-semibold">Custom</div>
            <div className="font-mono text-xs text-muted-foreground">set your own</div>
          </button>
        )}
      </div>

      {isCustom && (
        <div className="flex gap-3">
          <div className="flex-1 space-y-1.5">
            <Label className="text-xs">vCPU</Label>
            <Input
              aria-label="vCPU"
              value={value.cpu}
              onChange={(e) => onChange({ ...value, cpu: e.target.value })}
              placeholder="2"
              className="font-mono"
            />
          </div>
          <div className="flex-1 space-y-1.5">
            <Label className="text-xs">Memory</Label>
            <Input
              aria-label="Memory"
              value={value.memory}
              onChange={(e) => onChange({ ...value, memory: e.target.value })}
              placeholder="4Gi"
              className="font-mono"
            />
          </div>
        </div>
      )}
    </div>
  );
}
