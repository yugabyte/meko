// The one grant this lesson is about: which agent holds it, and exactly what
// that grant lets it do. The access levels are the product's real ones (see
// shareInviteRoles), not a lab-local invention — a user who reads this and
// then opens the datapack's own sharing page sees the same words.

import { Check } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  shareGrantDescription,
  shareGrantLabel,
} from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";

export function AgentGrantCard({
  agentDisplayName,
  agentEmail,
  grant,
  shared,
  sharing,
  onShare,
}: {
  agentDisplayName: string;
  agentEmail: string;
  /** The level the agent is granted — fixed by the lab, shown so the user
   *  knows what they're handing over. */
  grant: DatapackShareGrant;
  shared: boolean;
  sharing: boolean;
  onShare: () => void;
}) {
  return (
    <section className="flex flex-col gap-4 rounded-lg border border-border bg-card p-4">
      <div className="flex items-center gap-3">
        <span
          aria-hidden
          className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-foreground"
        >
          {agentDisplayName.slice(0, 2).toUpperCase()}
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-sm font-semibold text-foreground">{agentDisplayName}</p>
          <p className="truncate font-mono text-xs text-muted-foreground">{agentEmail}</p>
        </div>
        <Badge variant="secondary" className="shrink-0 text-[10px]">
          {shared ? shareGrantLabel(grant).toLowerCase() : "no access"}
        </Badge>
      </div>

      <dl className="flex flex-col divide-y divide-border border-y border-border">
        {/* One grant, stated once. Listing the levels it implies invited the
            reader to work out which one actually applies. */}
        <div className="flex items-start gap-3 py-3">
          <Check className="mt-0.5 h-4 w-4 shrink-0 text-primary" aria-hidden />
          <div className="min-w-0">
            <dt className="text-sm font-medium text-foreground">{shareGrantLabel(grant)}</dt>
            <dd className="text-xs text-muted-foreground">{shareGrantDescription(grant)}</dd>
          </div>
        </div>
      </dl>

      <Button onClick={onShare} disabled={shared || sharing} className="self-start">
        {shared ? "Shared ✓" : sharing ? "Sharing…" : "Share datapack"}
      </Button>
    </section>
  );
}
