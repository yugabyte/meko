import { Users } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { CurrentUser } from "@/lib/mekoApi";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";
import { Button } from "@/components/ui/button";

type Props = {
  currentUser: CurrentUser | null;
};

function displayName(user: CurrentUser): string {
  const full = `${user.first_name ?? ""} ${user.last_name ?? ""}`.trim();
  if (full) return full;
  return user.email ?? "You";
}

function MemberRow({ currentUser }: Props) {
  if (!currentUser?.email) {
    return (
      <p className="text-sm text-muted-foreground py-4 text-center">Loading member…</p>
    );
  }

  const initial = (displayName(currentUser).slice(0, 1) || "?").toUpperCase();

  return (
    <div className="flex items-center gap-3 rounded-lg border border-border bg-background/50 px-3 py-3">
      <div
        className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold text-foreground"
        aria-hidden
      >
        {initial}
      </div>
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm font-medium text-foreground">
          {displayName(currentUser)} <span className="text-muted-foreground font-normal">(you)</span>
        </div>
        <div className="truncate text-xs text-muted-foreground">{currentUser.email}</div>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        <Select defaultValue="admin">
          <SelectTrigger className="h-8 w-[140px] text-xs bg-background/80">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="admin">Admin</SelectItem>
            <SelectItem value="edit">Can edit memories</SelectItem>
            <SelectItem value="view">Can view</SelectItem>
          </SelectContent>
        </Select>
        <Button type="button" variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground" disabled aria-label="Remove member (only member)">
          <span className="text-lg leading-none" aria-hidden>
            ×
          </span>
        </Button>
      </div>
    </div>
  );
}

export function MembersSection({ currentUser }: Props) {
  return (
    <DatapackShareSectionCard
      title="Members"
      icon={<Users className="text-emerald-400" aria-hidden />}
      trailing={
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
          1
        </span>
      }
    >
      <Tabs defaultValue="all" className="w-full">
        <TabsList className="mb-4 h-9 w-full justify-start bg-muted/50 p-1 sm:w-auto">
          <TabsTrigger value="humans" className="text-xs sm:text-sm">
            Humans
          </TabsTrigger>
          <TabsTrigger value="agents" className="text-xs sm:text-sm">
            Agents
          </TabsTrigger>
          <TabsTrigger value="all" className="text-xs sm:text-sm">
            All
          </TabsTrigger>
        </TabsList>
        <TabsContent value="humans" className="mt-0 space-y-2">
          <MemberRow currentUser={currentUser} />
        </TabsContent>
        <TabsContent value="agents" className="mt-0">
          <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
            No agents yet.
          </p>
        </TabsContent>
        <TabsContent value="all" className="mt-0 space-y-2">
          <MemberRow currentUser={currentUser} />
        </TabsContent>
      </Tabs>
    </DatapackShareSectionCard>
  );
}
