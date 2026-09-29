// The one grant this lesson is about, drawn in the direction it travels:
// your datapack on the left, the agent receiving it on the right. The access
// levels are the product's real ones (see shareInviteRoles), not a lab-local
// invention — a user who reads this and then opens the datapack's own sharing
// page sees the same words.

import { ArrowRight, Check, Database } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  SHARE_INVITE_ROLES,
  shareGrantDescription,
  shareGrantLabel,
} from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";

export function AgentGrantCard({
  datapackName,
  agentDisplayName,
  agentEmail,
  grant,
  shared,
  sharing,
  inviteSent,
  onShare,
}: {
  /** The datapack being handed over. Named, not implied: the reader has two
   *  or three of these and needs to see which one the grant covers. */
  datapackName: string;
  agentDisplayName: string;
  agentEmail: string;
  /** The level the agent is granted — fixed by the lab, shown so the user
   *  knows what they're handing over. */
  grant: DatapackShareGrant;
  shared: boolean;
  sharing: boolean;
  /** An invite exists but is not accepted yet, so the agent still has no
   *  access. Re-sending it is rejected as a duplicate, so the button stops
   *  offering to, but this is NOT the granted state. */
  inviteSent?: boolean;
  onShare: () => void;
}) {
  return (
    <section className="flex flex-col gap-4 rounded-lg border border-border bg-card p-4">
      <div className="flex items-center gap-3">
        <div className="flex min-w-0 flex-1 items-center gap-2 rounded-md border border-border bg-background/40 p-3">
          <Database className="h-4 w-4 shrink-0 text-primary" aria-hidden />
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-foreground">{datapackName}</p>
            <p className="text-[11px] text-muted-foreground">your datapack</p>
          </div>
        </div>

        <ArrowRight className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />

        <div className="flex min-w-0 flex-1 items-center gap-2 rounded-md border border-border bg-background/40 p-3">
          <span
            aria-hidden
            className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-foreground"
          >
            {agentDisplayName.slice(0, 2).toUpperCase()}
          </span>
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-foreground">{agentDisplayName}</p>
            <p className="truncate font-mono text-[11px] text-muted-foreground">{agentEmail}</p>
          </div>
        </div>

        {/* The real role list, with the other two visible but disabled: the
            lab fixes this grant at contributor, and hiding the levels either
            side made it look like the only one that exists. Seeing them
            greyed says "this is a choice the product offers, and the lab has
            made it for you". */}
        {/* Openable on purpose, and controlled with no onValueChange: the
            reader can look at the three levels, and picking another does
            nothing. Disabling the whole Select would have hidden the very
            list this is here to show. */}
        <Select value={grant}>
          <SelectTrigger aria-label="Access level" className="h-8 w-[9.5rem] shrink-0 text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {SHARE_INVITE_ROLES.map((role) => (
              <SelectItem key={role.id} value={role.id} disabled={role.id !== grant}>
                {role.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Badge
          variant={shared ? "success" : inviteSent ? "outline" : "secondary"}
          className="shrink-0 text-[10px]"
        >
          {shared ? "granted" : inviteSent ? "invite pending" : "no access"}
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

      <Button
        busy={sharing}
        busyLabel="Sharing"
        done={shared}
        doneLabel={`Shared with ${agentDisplayName}`}
        disabled={inviteSent}
        onClick={onShare}
        className="self-start"
      >
        {inviteSent ? "Invite sent, not accepted" : `Share with ${agentDisplayName}`}
      </Button>
    </section>
  );
}
