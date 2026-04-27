import { useState } from "react";
import { Mail } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SHARE_INVITE_ROLES, type ShareInviteRoleId } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";

export function InviteByEmailSection() {
  const [inviteRole, setInviteRole] = useState<ShareInviteRoleId>("edit_memories");
  const [tileRole, setTileRole] = useState<ShareInviteRoleId>("edit_memories");

  const syncSelectFromTiles = (id: ShareInviteRoleId) => {
    setTileRole(id);
    setInviteRole(id);
  };

  return (
    <DatapackShareSectionCard title="Invite by email" icon={<Mail className="text-blue-400" aria-hidden />}>
      <div className="flex flex-col gap-4 lg:flex-row lg:items-end">
        <div className="min-w-0 flex-1 space-y-2">
          <label htmlFor="share-invite-emails" className="sr-only">
            Email addresses
          </label>
          <Input
            id="share-invite-emails"
            placeholder="name@company.com, one or more…"
            className="bg-background/80"
            autoComplete="off"
          />
        </div>
        <div className="flex w-full flex-col gap-2 sm:flex-row sm:items-center lg:w-auto">
          <Select
            value={inviteRole}
            onValueChange={(v) => {
              const id = v as ShareInviteRoleId;
              setInviteRole(id);
              setTileRole(id);
            }}
          >
            <SelectTrigger className="w-full sm:w-[200px] bg-background/80">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {SHARE_INVITE_ROLES.map((r) => (
                <SelectItem key={r.id} value={r.id}>
                  {r.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex w-full shrink-0 sm:w-auto" tabIndex={0}>
                <Button
                  type="button"
                  disabled
                  className="w-full shrink-0 cursor-not-allowed bg-primary text-primary-foreground opacity-70 sm:w-auto"
                >
                  Send invite
                </Button>
              </span>
            </TooltipTrigger>
            <TooltipContent side="top">Coming Soon</TooltipContent>
          </Tooltip>
        </div>
      </div>

      <div className="mt-6 grid gap-3 sm:grid-cols-3">
        {SHARE_INVITE_ROLES.map((r) => {
          const Icon = r.icon;
          const selected = tileRole === r.id;
          return (
            <button
              key={r.id}
              type="button"
              onClick={() => syncSelectFromTiles(r.id)}
              className={cn(
                "flex flex-col gap-2 rounded-xl border p-4 text-left transition-colors",
                selected
                  ? "border-blue-500/60 bg-blue-500/10 shadow-[inset_0_0_0_1px_rgba(59,130,246,0.35)]"
                  : "border-border bg-muted/20 hover:border-muted-foreground/30 hover:bg-muted/30",
              )}
            >
              <Icon className={cn("h-5 w-5", selected ? "text-blue-400" : "text-muted-foreground")} aria-hidden />
              <span className="text-sm font-semibold text-foreground">{r.label}</span>
              <span className="text-xs leading-snug text-muted-foreground">{r.description}</span>
            </button>
          );
        })}
      </div>
    </DatapackShareSectionCard>
  );
}
