// SchedulingField — edit an endpoint's tolerations and topology spread
// constraints as rows, or as JSON for the fields the rows do not cover
// (matchExpressions, minDomains, …). Presentation-only: the draft and its
// validation live in lib/scheduling. Shared by the create forms and the edit
// dialog so the three cannot drift.

import { useState } from "react";
import { ChevronRight, Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import type { TaintEffect, TolerationOperator, WhenUnsatisfiable } from "@/lib/api/types";
import { dataBranchingMessages as msg } from "@/lib/messages";
import {
  buildScheduling,
  newSpreadRow,
  newTolerationRow,
  switchMode,
  TAINT_EFFECTS,
  TOLERATION_OPERATORS,
  WHEN_UNSATISFIABLE,
  type SchedulingDraft,
  type SpreadRow,
  type TolerationRow,
} from "@/lib/scheduling";
import { cn } from "@/lib/utils";

// Radix Select refuses an empty-string item value.
const ANY_EFFECT = "any";

const small = "h-8 font-mono text-xs";
const caption = "text-[10px] uppercase tracking-wide text-muted-foreground";

export function SchedulingField({
  value,
  onChange,
  defaultSelector,
}: {
  value: SchedulingDraft;
  onChange: (v: SchedulingDraft) => void;
  /** Prefills a new spread constraint's label selector. */
  defaultSelector: Record<string, string>;
}) {
  const built = buildScheduling(value);
  const [switchError, setSwitchError] = useState(false);

  function setMode(mode: SchedulingDraft["mode"]) {
    const next = switchMode(value, mode);
    setSwitchError(!next.ok);
    if (next.ok) onChange(next.value);
  }

  const setTol = (i: number, patch: Partial<TolerationRow>) =>
    onChange({
      ...value,
      tolerations: value.tolerations.map((r, j) => (j === i ? { ...r, ...patch } : r)),
    });
  const setSpread = (i: number, patch: Partial<SpreadRow>) =>
    onChange({
      ...value,
      spread: value.spread.map((r, j) => (j === i ? { ...r, ...patch } : r)),
    });

  return (
    <div className="space-y-3">
      <div className="flex gap-1" role="group" aria-label="Scheduling editor">
        {(["form", "json"] as const).map((m) => (
          <Button
            key={m}
            type="button"
            size="sm"
            variant={value.mode === m ? "secondary" : "ghost"}
            aria-pressed={value.mode === m}
            className="h-7 px-2 text-xs"
            onClick={() => setMode(m)}
          >
            {m === "form" ? "Form" : "JSON"}
          </Button>
        ))}
      </div>

      {value.mode === "json" ? (
        <Textarea
          aria-label="Scheduling JSON"
          value={value.json}
          onChange={(e) => onChange({ ...value, json: e.target.value })}
          rows={10}
          spellCheck={false}
          placeholder='{"tolerations": [], "topology_spread_constraints": []}'
          className="font-mono text-xs"
        />
      ) : (
        <>
          <div className="space-y-2">
            <Label className="text-xs">Tolerations</Label>
            {value.tolerations.length === 0 && (
              <p className="text-xs text-muted-foreground">{msg.schedulingNoTolerations}</p>
            )}
            {value.tolerations.map((t, i) => (
              <div key={i} className="flex flex-wrap items-end gap-2">
                <div className="flex flex-col gap-1">
                  <span className={caption}>Key</span>
                  <Input
                    aria-label={`Toleration ${i + 1} key`}
                    value={t.key}
                    onChange={(e) => setTol(i, { key: e.target.value })}
                    placeholder="any key"
                    className={cn(small, "w-40")}
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <span className={caption}>Operator</span>
                  <Select
                    value={t.operator}
                    onValueChange={(v) =>
                      setTol(i, {
                        operator: v as TolerationOperator,
                        // Exists carries no value; clear it rather than only
                        // disabling the box, or it would still be sent.
                        ...(v === "Exists" ? { value: "" } : {}),
                      })
                    }
                  >
                    <SelectTrigger aria-label={`Toleration ${i + 1} operator`} className="h-8 w-24">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {TOLERATION_OPERATORS.map((o) => (
                        <SelectItem key={o} value={o}>
                          {o}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="flex flex-col gap-1">
                  <span className={caption}>Value</span>
                  <Input
                    aria-label={`Toleration ${i + 1} value`}
                    value={t.value}
                    disabled={t.operator === "Exists"}
                    onChange={(e) => setTol(i, { value: e.target.value })}
                    className={cn(small, "w-28")}
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <span className={caption}>Effect</span>
                  <Select
                    value={t.effect || ANY_EFFECT}
                    onValueChange={(v) => {
                      const effect = v === ANY_EFFECT ? "" : (v as TaintEffect);
                      setTol(i, {
                        effect,
                        ...(effect === "NoExecute" ? {} : { seconds: "" }),
                      });
                    }}
                  >
                    <SelectTrigger aria-label={`Toleration ${i + 1} effect`} className="h-8 w-36">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value={ANY_EFFECT}>any effect</SelectItem>
                      {TAINT_EFFECTS.map((e) => (
                        <SelectItem key={e} value={e}>
                          {e}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                {t.effect === "NoExecute" && (
                  <div className="flex flex-col gap-1">
                    <span className={caption}>Seconds</span>
                    <Input
                      aria-label={`Toleration ${i + 1} seconds`}
                      value={t.seconds}
                      onChange={(e) => setTol(i, { seconds: e.target.value })}
                      placeholder="forever"
                      className={cn(small, "w-20")}
                    />
                  </div>
                )}
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  aria-label={`Remove toleration ${i + 1}`}
                  className="h-8 px-2"
                  onClick={() =>
                    onChange({ ...value, tolerations: value.tolerations.filter((_, j) => j !== i) })
                  }
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </Button>
              </div>
            ))}
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="h-7 gap-1 text-xs"
              onClick={() =>
                onChange({ ...value, tolerations: [...value.tolerations, newTolerationRow()] })
              }
            >
              <Plus className="h-3.5 w-3.5" />
              Add toleration
            </Button>
          </div>

          <div className="space-y-2">
            <Label className="text-xs">Topology spread constraints</Label>
            {value.spread.length === 0 && (
              <p className="text-xs text-muted-foreground">{msg.schedulingNoSpread}</p>
            )}
            {value.spread.map((c, i) => {
              const extra = Object.keys(c.extra);
              return (
                <div key={i} className="space-y-2 rounded-md border border-border p-2">
                  <div className="flex flex-wrap items-end gap-2">
                    <div className="flex flex-col gap-1">
                      <span className={caption}>Max skew</span>
                      <Input
                        aria-label={`Spread ${i + 1} max skew`}
                        value={c.maxSkew}
                        onChange={(e) => setSpread(i, { maxSkew: e.target.value })}
                        className={cn(small, "w-16")}
                      />
                    </div>
                    <div className="flex flex-col gap-1">
                      <span className={caption}>Topology key</span>
                      <Input
                        aria-label={`Spread ${i + 1} topology key`}
                        value={c.topologyKey}
                        onChange={(e) => setSpread(i, { topologyKey: e.target.value })}
                        className={cn(small, "w-56")}
                      />
                    </div>
                    <div className="flex flex-col gap-1">
                      <span className={caption}>When unsatisfiable</span>
                      <Select
                        value={c.whenUnsatisfiable}
                        onValueChange={(v) =>
                          setSpread(i, { whenUnsatisfiable: v as WhenUnsatisfiable })
                        }
                      >
                        <SelectTrigger
                          aria-label={`Spread ${i + 1} when unsatisfiable`}
                          className="h-8 w-40"
                        >
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          {WHEN_UNSATISFIABLE.map((w) => (
                            <SelectItem key={w} value={w}>
                              {w}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    </div>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-label={`Remove spread constraint ${i + 1}`}
                      className="h-8 px-2"
                      onClick={() =>
                        onChange({ ...value, spread: value.spread.filter((_, j) => j !== i) })
                      }
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                  <div className="space-y-1">
                    <span className={caption}>Selects pods labelled</span>
                    {c.labels.map((l, k) => (
                      <div key={k} className="flex items-center gap-1">
                        <Input
                          aria-label={`Spread ${i + 1} label ${k + 1} key`}
                          value={l.key}
                          onChange={(e) =>
                            setSpread(i, {
                              labels: c.labels.map((x, j) =>
                                j === k ? { ...x, key: e.target.value } : x,
                              ),
                            })
                          }
                          className={cn(small, "min-w-0 flex-1")}
                        />
                        <span className="text-xs text-muted-foreground">=</span>
                        <Input
                          aria-label={`Spread ${i + 1} label ${k + 1} value`}
                          value={l.value}
                          onChange={(e) =>
                            setSpread(i, {
                              labels: c.labels.map((x, j) =>
                                j === k ? { ...x, value: e.target.value } : x,
                              ),
                            })
                          }
                          className={cn(small, "min-w-0 flex-1")}
                        />
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          aria-label={`Remove spread ${i + 1} label ${k + 1}`}
                          className="h-8 px-2"
                          onClick={() =>
                            setSpread(i, { labels: c.labels.filter((_, j) => j !== k) })
                          }
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </div>
                    ))}
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-7 gap-1 px-2 text-xs"
                      onClick={() =>
                        setSpread(i, { labels: [...c.labels, { key: "", value: "" }] })
                      }
                    >
                      <Plus className="h-3.5 w-3.5" />
                      Add label
                    </Button>
                    {extra.length > 0 && (
                      <p className="text-[11px] text-muted-foreground">
                        {msg.schedulingExtraFields(extra.join(", "))}
                      </p>
                    )}
                  </div>
                </div>
              );
            })}
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="h-7 gap-1 text-xs"
              onClick={() =>
                onChange({ ...value, spread: [...value.spread, newSpreadRow(defaultSelector)] })
              }
            >
              <Plus className="h-3.5 w-3.5" />
              Add spread constraint
            </Button>
          </div>
        </>
      )}

      {!built.ok && (
        <p role="alert" className="text-[11px] text-destructive">
          {built.error}
        </p>
      )}
      {switchError && !built.ok && (
        <p className="text-[11px] text-muted-foreground">{msg.schedulingFixBeforeSwitch}</p>
      )}
    </div>
  );
}

/** SchedulingField behind a collapsed-by-default disclosure, for the create forms. */
export function SchedulingSection(props: Parameters<typeof SchedulingField>[0]) {
  const built = buildScheduling(props.value);
  const count = built.ok
    ? built.value.tolerations.length + built.value.topology_spread_constraints.length
    : 0;
  return (
    <Collapsible className="group">
      <CollapsibleTrigger asChild>
        <Button type="button" variant="ghost" size="sm" className="h-7 gap-1 px-1 text-xs">
          <ChevronRight className="h-3.5 w-3.5 transition-transform group-data-[state=open]:rotate-90" />
          {msg.schedulingSection}
          {count > 0 && <span className="text-muted-foreground">({count})</span>}
          {!built.ok && <span className="text-destructive">(invalid)</span>}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="pt-2">
        <SchedulingField {...props} />
      </CollapsibleContent>
    </Collapsible>
  );
}
