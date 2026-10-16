// Edit one compute's tolerations and topology spread constraints.
//
// Only a list the operator changed is sent: the controller keeps an omitted list
// and clears one sent as `[]`, and either change recreates the pod, so echoing
// an untouched list back would at best be a pointless restart.

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
import { SchedulingField } from "@/components/ui/scheduling-field";
import { WarningBanner } from "@/components/ui/warning-banner";
import { useUpdateEndpointScheduling } from "@/hooks/useEndpoints";
import type { Endpoint, UpdateEndpointSchedulingInput } from "@/lib/api/types";
import { dataBranchingMessages as msg } from "@/lib/messages";
import {
  buildScheduling,
  CLUSTER_ID_LABEL,
  draftOf,
  EMPTY_SCHEDULING,
  type SchedulingValue,
} from "@/lib/scheduling";

export function SchedulingDialog({
  open,
  onOpenChange,
  poolId,
  ep,
  title,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  poolId: string;
  ep: Endpoint;
  title: string;
}) {
  const [draft, setDraft] = useState(EMPTY_SCHEDULING);
  const [initial, setInitial] = useState<SchedulingValue | null>(null);
  const update = useUpdateEndpointScheduling();
  const built = buildScheduling(draft);

  // Snapshot on open only: a background refetch hands a new `ep` and would
  // otherwise wipe what is being typed.
  const openedRef = useRef(false);
  useLayoutEffect(() => {
    if (!open) {
      openedRef.current = false;
      return;
    }
    if (openedRef.current) return;
    openedRef.current = true;
    const d = draftOf(ep.tolerations, ep.topology_spread_constraints);
    setDraft(d);
    const b = buildScheduling(d);
    setInitial(b.ok ? b.value : null);
  }, [open, ep]);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!built.ok) return;
    const scheduling: UpdateEndpointSchedulingInput = {};
    const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
    if (!initial || !same(built.value.tolerations, initial.tolerations)) {
      scheduling.tolerations = built.value.tolerations;
    }
    if (
      !initial ||
      !same(built.value.topology_spread_constraints, initial.topology_spread_constraints)
    ) {
      scheduling.topology_spread_constraints = built.value.topology_spread_constraints;
    }
    if (Object.keys(scheduling).length === 0) {
      onOpenChange(false);
      return;
    }
    update.mutate(
      { poolId, clusterId: ep.cluster_id, branchId: ep.branch_id, endpointId: ep.id, scheduling },
      {
        onSuccess: () => {
          toast.success(msg.schedulingSaved);
          onOpenChange(false);
        },
        onError: (err) => toast.error(err instanceof Error ? err.message : msg.schedulingError),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !update.isPending && onOpenChange(o)}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>
              {msg.schedulingTitle} — {title}
            </DialogTitle>
            <DialogDescription>{msg.schedulingDescription}</DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <SchedulingField
              value={draft}
              onChange={setDraft}
              defaultSelector={{ [CLUSTER_ID_LABEL]: ep.cluster_id }}
            />
            <WarningBanner>{msg.schedulingRecreateWarning}</WarningBanner>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={update.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={update.isPending || !built.ok}>
              {update.isPending ? "Recreating…" : "Save and recreate"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
