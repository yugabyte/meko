// Confirmation card for agentic-chat pending actions (MEKO-407 Phase 2).
// Sits directly above the chat input (inline Allow/Deny): the model
// proposed a confirm-tier tool call, the gateway froze it without executing,
// and this card resolves it via POST /chat/confirm-action.

import { Loader2, ShieldQuestion } from "lucide-react";
import type { PendingAction } from "@/lib/inferenceGateway";

/** Human-readable one-liner for the proposed action. Falls back to the raw
 *  tool name so unknown/future tools still render something sensible. */
export function describePendingAction(action: PendingAction): string {
  const args = action.args ?? {};
  switch (action.tool_name) {
    case "memory_promote": {
      const ids = Array.isArray(args.memory_ids) ? (args.memory_ids as string[]) : [];
      return ids.length > 0
        ? `Promote ${ids.length === 1 ? `memory "${ids[0]}"` : `${ids.length} memories`} to the team knowledge base`
        : "Promote memories to the team knowledge base";
    }
    case "memory_delete_by_id":
      return args.memory_id ? `Delete memory "${String(args.memory_id)}"` : "Delete a memory";
    case "datapack_update": {
      const parts: string[] = [];
      if (args.name) parts.push(`rename to "${String(args.name)}"`);
      if (args.description) parts.push("update the description");
      return parts.length > 0
        ? `Update this datapack: ${parts.join(" and ")}`
        : "Update this datapack";
    }
    case "amp_cluster_create":
      return args.name
        ? `Create Postgres cluster "${String(args.name)}"`
        : "Create a Postgres cluster";
    case "amp_cluster_delete":
      return "Delete a Postgres cluster";
    case "amp_branch_create":
      return args.name ? `Create branch "${String(args.name)}"` : "Create a branch";
    case "amp_branch_delete":
      return "Delete a branch";
    case "amp_compute_create":
      return "Create a compute endpoint";
    case "amp_compute_delete":
      return "Delete a compute endpoint";
    default:
      return `Run ${action.tool_name}`;
  }
}

/** Past-tense line appended to the transcript after a successful Allow —
 *  e.g. `Memory "1234" promoted to the team knowledge base.` */
export function describeActionResult(action: PendingAction): string {
  const args = action.args ?? {};
  switch (action.tool_name) {
    case "memory_promote": {
      const ids = Array.isArray(args.memory_ids) ? (args.memory_ids as string[]) : [];
      return ids.length === 1
        ? `Memory "${ids[0]}" promoted to the team knowledge base.`
        : `${ids.length || "The"} memories promoted to the team knowledge base.`;
    }
    case "memory_delete_by_id":
      return args.memory_id ? `Memory "${String(args.memory_id)}" deleted.` : "Memory deleted.";
    case "datapack_update":
      return "Datapack updated.";
    case "amp_cluster_create":
      return args.name
        ? `Postgres cluster "${String(args.name)}" created.`
        : "Postgres cluster created.";
    case "amp_cluster_delete":
      return "Postgres cluster deleted.";
    case "amp_branch_create":
      return args.name ? `Branch "${String(args.name)}" created.` : "Branch created.";
    case "amp_branch_delete":
      return "Branch deleted.";
    case "amp_compute_create":
      return "Compute endpoint created.";
    case "amp_compute_delete":
      return "Compute endpoint deleted.";
    default:
      return `${action.tool_name} completed.`;
  }
}

type Props = {
  action: PendingAction;
  /** True while the confirm/deny request is in flight — both buttons lock. */
  busy: boolean;
  onAllow: () => void;
  onDeny: () => void;
};

export function PendingActionCard({ action, busy, onAllow, onDeny }: Props) {
  return (
    <div className="mx-auto w-full max-w-3xl px-6 pb-2">
      <div className="flex items-center gap-3 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2.5">
        <ShieldQuestion className="h-4 w-4 shrink-0 text-primary" aria-hidden />
        <div className="min-w-0 flex-1">
          <p className="text-sm text-foreground">{describePendingAction(action)}?</p>
          <p className="text-[11px] text-muted-foreground">Nothing happens until you allow it.</p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <button
            type="button"
            onClick={onDeny}
            disabled={busy}
            className="rounded-md border border-border bg-card px-3 py-1.5 text-xs text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
          >
            Deny
          </button>
          <button
            type="button"
            onClick={onAllow}
            disabled={busy}
            className="inline-flex items-center gap-1.5 rounded-md bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:opacity-50"
          >
            {busy ? <Loader2 className="h-3 w-3 animate-spin" aria-hidden /> : null}
            Allow
          </button>
        </div>
      </div>
    </div>
  );
}
