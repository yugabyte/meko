// Edit the settings of a branch or one compute.
//
// Each surface edits as key/value rows: an operator can see what is set at a
// glance, and adding one setting does not mean getting the punctuation of a
// whole JSON document right. Value typing lives in SettingsRows — the reason a
// grid is not simply a map of strings.
//
// Each surface opens showing what is stored, so the dialog answers "what is
// set?" as well as "change it" — the two questions an operator has, and the
// reason a blank form was wrong: it could not distinguish "nothing is set" from
// "we are not telling you", and every edit meant retyping what you could not
// see.
//
// Only surfaces the operator actually *changed* are sent. An untouched one is
// omitted, which the controller reads as "leave it alone"; a surface emptied of
// rows sends `{}`, which clears it. Sending every surface on every save would
// rewrite the others with whatever the dialog read on open — stale by exactly
// the window it was up.

import { useLayoutEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { useUpdateBranchSettings } from "@/lib/api/queries";
import { useUpdateEndpointSettings } from "@/hooks/useEndpoints";
import type { SettingsUpdateResult } from "@/lib/api/types";
import { objectOf, rowsOf, SettingsRows, type Row, type ValueKind } from "./SettingsRows";
import { dataBranchingMessages as msg } from "@/lib/messages";

/** One editable surface: what it is called, and how its values are typed. */
type Surface = {
  key: "pg_settings" | "pageserver_settings" | "safekeeper_settings" | "write_token_settings";
  label: string;
  kind: ValueKind;
};

// No example values anywhere: a placeholder that reads like a real setting is
// indistinguishable from one, so an unset surface looked configured.
const BRANCH_SURFACES: Surface[] = [
  { key: "pg_settings", label: "Postgres settings", kind: "string" },
  { key: "write_token_settings", label: "Write lease", kind: "number" },
];

const ENDPOINT_SURFACES: Surface[] = [
  { key: "pg_settings", label: "Postgres settings", kind: "string" },
  { key: "pageserver_settings", label: "Pageserver settings", kind: "scalar" },
  { key: "safekeeper_settings", label: "Safekeeper settings", kind: "scalar" },
];

/** Turn the reply into one line.
 *
 * A restart is the part with a cost — it drops that endpoint's Postgres
 * connections — so it is never dropped from the message. An earlier version
 * reported unreachability first and returned, which hid "4 computes restarted"
 * behind "1 could not be reached": the loudest fact was the one the operator
 * most needed and least got.
 */
function outcomeMessage(result: SettingsUpdateResult): string {
  const outcomes = result.endpoints ?? [];
  const base = appliedMessage(result);
  // A setting nothing can apply is the one outcome the operator cannot see for
  // themselves: no restart, no error, and the value sitting in the row as though
  // it took. Appended rather than substituted, because it does not make the
  // restart it accompanied any less worth reporting.
  // Grouped by reason, so one line reads "x, y: unrecognized parameter" rather
  // than repeating the explanation per setting.
  const byReason = new Map<string, string[]>();
  for (const { name, reason } of outcomes.flatMap((o) => o.not_applied ?? [])) {
    const names = byReason.get(reason) ?? [];
    if (!names.includes(name)) names.push(name);
    byReason.set(reason, names);
  }
  if (byReason.size === 0) return base;
  const detail = [...byReason]
    .map(([reason, names]) => `${names.join(", ")} (${reason})`)
    .join("; ");
  return msg.settingsSavedNotApplied(base, detail);
}

function appliedMessage(result: SettingsUpdateResult): string {
  const outcomes = result.endpoints ?? [];
  const restarted = [...new Set(outcomes.flatMap((o) => o.restarted ?? []))];
  const reloaded = [...new Set(outcomes.flatMap((o) => o.reloaded ?? []))];
  const forcedBy = [...new Set(outcomes.flatMap((o) => o.restart_forced_by ?? []))];
  const unreachable = outcomes.filter((o) => o.unreachable).length;
  if (restarted.length > 0 && unreachable > 0) {
    return msg.settingsSavedRestartedAndUnreachable(restarted.join(", "), unreachable);
  }
  if (unreachable > 0) return msg.settingsSavedUnreachable;
  // Naming the setting that forced it turns "we restarted your database" from
  // an arbitrary cost into one the operator can predict next time.
  if (restarted.length > 0 && forcedBy.length > 0) {
    return msg.settingsSavedRestartedBecause(restarted.join(", "), forcedBy.join(", "));
  }
  if (restarted.length > 0) return msg.settingsSavedRestarted(restarted.join(", "));
  // A reload is a success, not a no-op: saying "nothing needed restarting"
  // would understate that the change is in force.
  if (reloaded.length > 0) return msg.settingsSavedReloaded;
  return msg.settingsSavedNoRestart;
}

export function SettingsDialog({
  open,
  onOpenChange,
  poolId,
  clusterId,
  branchId,
  endpointId,
  stored,
  title,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  poolId: string;
  clusterId: string;
  branchId: string;
  /** Omit to edit the branch's settings; pass one to edit that compute's. */
  endpointId?: string;
  /** What is stored now, keyed by surface — shown in the boxes and used as the
   *  baseline for "did this change?". A surface absent here reads as unset. */
  stored?: Partial<Record<Surface["key"], unknown>>;
  title: string;
}) {
  const surfaces = endpointId ? ENDPOINT_SURFACES : BRANCH_SURFACES;
  const [rows, setRows] = useState<Record<string, Row[]>>({});
  // What each surface was opened with, serialised for comparison. This is what
  // makes an untouched surface omitted rather than echoed back.
  const [initial, setInitial] = useState<Record<string, string>>({});
  const updateBranch = useUpdateBranchSettings();
  const updateEndpoint = useUpdateEndpointSettings();
  const pending = updateBranch.isPending || updateEndpoint.isPending;

  // Snapshot what is stored when the dialog OPENS, and only then.
  //
  // Deliberately not keyed on `stored`: the parent builds that object inline, so
  // it is a new identity on every parent render, and re-reading on identity
  // would wipe half-typed input the moment anything else re-rendered — a
  // react-query refetch on window focus is enough. Reopening re-reads, which is
  // when a value changed elsewhere should appear.
  // useLayoutEffect, not useEffect: a passive effect runs after paint, so the
  // dialog would flash "Nothing set." under every surface before the stored
  // values appeared — the one message it must never show falsely, since telling
  // "nothing is set" from "we are not telling you" is the reason it prefills.
  const openedRef = useRef(false);
  useLayoutEffect(() => {
    if (!open) {
      openedRef.current = false;
      return;
    }
    if (openedRef.current) return;
    openedRef.current = true;
    const nextRows: Record<string, Row[]> = {};
    const nextInitial: Record<string, string> = {};
    for (const s of surfaces) {
      const r = rowsOf(stored?.[s.key]);
      nextRows[s.key] = r;
      const built = objectOf(r, s.kind);
      nextInitial[s.key] = JSON.stringify(built.ok ? built.value : {});
    }
    setRows(nextRows);
    setInitial(nextInitial);
  }, [open, stored, surfaces]);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const settings: Record<string, unknown> = {};
    for (const s of surfaces) {
      const built = objectOf(rows[s.key] ?? [], s.kind);
      if (!built.ok) {
        // Name the row and what is wrong with it: the operator is looking at a
        // list and needs to know which line to fix.
        const { kind, key } = built.error;
        toast.error(
          kind === "blank"
            ? msg.settingsBlankValue(s.label, key)
            : kind === "duplicate"
              ? msg.settingsDuplicateKey(s.label, key)
              : msg.settingsInvalidValue(s.label, key),
        );
        return;
      }
      // Unchanged: leave that surface alone rather than echoing it back.
      if (JSON.stringify(built.value) === (initial[s.key] ?? "{}")) continue;
      settings[s.key] = built.value;
    }
    if (Object.keys(settings).length === 0) {
      onOpenChange(false);
      return;
    }

    const onSuccess = (result: SettingsUpdateResult) => {
      toast.success(outcomeMessage(result));
      onOpenChange(false);
    };
    const onError = (err: unknown) =>
      toast.error(err instanceof Error ? err.message : msg.settingsError);

    if (endpointId) {
      updateEndpoint.mutate(
        { poolId, clusterId, branchId, endpointId, settings },
        { onSuccess, onError },
      );
    } else {
      updateBranch.mutate({ poolId, clusterId, branchId, settings }, { onSuccess, onError });
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>
              {msg.settingsTitle} — {title}
            </DialogTitle>
            <DialogDescription>
              {endpointId ? msg.settingsEndpointDescription : msg.settingsBranchDescription}
            </DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            {surfaces.map((s) => (
              <div key={s.key} className="space-y-2">
                <Label id={`settings-${s.key}-label`}>{s.label}</Label>
                <SettingsRows
                  id={`settings-${s.key}`}
                  rows={rows[s.key] ?? []}
                  kind={s.kind}
                  onChange={(next) => setRows((r) => ({ ...r, [s.key]: next }))}
                />
              </div>
            ))}
            <p className="text-muted-foreground text-sm">
              {endpointId ? msg.settingsRestartWarning : msg.settingsRestartWarningBranch}
            </p>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={pending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={pending}>
              {pending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
