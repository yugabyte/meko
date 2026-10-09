// Admin Users page: lists all users and lets an admin move each user along the
// tier ladder (free → standard → pro, and back down), confirming through an
// AlertDialog first. Enterprise is sales-led and not settable here.
import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { format, parseISO } from "date-fns";
import { toast } from "sonner";
import { History, Search } from "lucide-react";
import axios from "axios";
import {
  fetchAdminUsers,
  fetchUserTierAudit,
  updateUserTier,
  type AdminUser,
  type Tier,
} from "@/lib/adminApi";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

type TierFilter = "all" | "free" | "standard" | "pro";

const LABEL: Record<string, string> = {
  free: "Free",
  standard: "Standard",
  pro: "Pro",
  enterprise: "Enterprise",
};

// The tiers the admin console can set, low → high (matches api_server's
// validTiers). Each user's dropdown offers all of these except their current
// tier, so an admin can move directly to any of them. Enterprise isn't settable
// here (sales-led), but an enterprise user can be moved down to any of these.
const TIER_LADDER = ["free", "standard", "pro"] as const;
const PAGE_SIZE = 12;

function errMessage(e: unknown, fallback: string): string {
  if (axios.isAxiosError(e)) {
    const m = (e.response?.data as { message?: string } | undefined)?.message;
    return m ?? e.message;
  }
  return e instanceof Error ? e.message : fallback;
}

// Full name from first/last, falling back to the email local-part.
function displayName(u: AdminUser): string {
  const name = `${u.first_name ?? ""} ${u.last_name ?? ""}`.trim();
  return name || u.email;
}

// Up to two initials from the name, falling back to the email's first letter.
function initials(u: AdminUser): string {
  const first = u.first_name?.trim()?.[0] ?? "";
  const last = u.last_name?.trim()?.[0] ?? "";
  const combined = `${first}${last}`.trim();
  return (combined || u.email?.[0] || "?").toUpperCase();
}

function TierBadge({ tier }: { tier: Tier }) {
  switch (tier) {
    case "pro":
      return (
        <Badge
          variant="outline"
          className="border-transparent bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
        >
          Pro
        </Badge>
      );
    case "standard":
      return (
        <Badge
          variant="outline"
          className="border-transparent bg-blue-500/15 text-blue-600 dark:text-blue-400"
        >
          Standard
        </Badge>
      );
    case "enterprise":
      return (
        <Badge
          variant="outline"
          className="border-transparent bg-violet-500/15 text-violet-600 dark:text-violet-400"
        >
          Enterprise
        </Badge>
      );
    default:
      return <Badge variant="secondary">Free</Badge>;
  }
}

export function UsersPage() {
  const [filter, setFilter] = useState<TierFilter>("all");
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  // The user + target tier awaiting confirmation in the dialog.
  const [pending, setPending] = useState<{ user: AdminUser; tier: string } | null>(null);
  // The user whose tier-change history is open.
  const [historyUser, setHistoryUser] = useState<AdminUser | null>(null);
  const queryClient = useQueryClient();

  const historyQuery = useQuery({
    queryKey: ["admin-tier-audit", historyUser?.id],
    queryFn: () => fetchUserTierAudit(historyUser!.id),
    enabled: historyUser !== null,
  });

  const usersQuery = useQuery({
    queryKey: ["admin-users"],
    queryFn: fetchAdminUsers,
    staleTime: 60_000,
  });

  const mutation = useMutation({
    mutationFn: ({ userId, tier }: { userId: string; tier: string }) =>
      updateUserTier(userId, tier),
    onSuccess: (_data, { tier }) => {
      toast.success(`Changed ${pending?.user.email ?? "user"} to ${LABEL[tier] ?? tier}`);
      queryClient.invalidateQueries({ queryKey: ["admin-users"] });
      queryClient.invalidateQueries({ queryKey: ["admin-usage"] });
    },
    onError: (e) => toast.error(errMessage(e, "Could not update tier.")),
    onSettled: () => setPending(null),
  });

  const users = usersQuery.data;
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return (users ?? []).filter((u) => {
      if (filter !== "all" && u.tier !== filter) return false;
      if (!q) return true;
      const haystack = `${u.email} ${u.first_name ?? ""} ${u.last_name ?? ""}`.toLowerCase();
      return haystack.includes(q);
    });
  }, [users, filter, search]);

  // Search / filter changes should always land the user back on the first page.
  useEffect(() => {
    setPage(1);
  }, [filter, search]);

  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  // Clamp in case the filtered list shrank below the current page.
  const currentPage = Math.min(page, pageCount);
  const start = (currentPage - 1) * PAGE_SIZE;
  const pageRows = filtered.slice(start, start + PAGE_SIZE);
  const showPagination = filtered.length > PAGE_SIZE;

  const isFiltering = search.trim() !== "" || filter !== "all";

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Users</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Manage user tiers.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <InputGroup className="w-56 border border-border bg-card">
            <InputGroupAddon>
              <Search className="h-4 w-4" />
            </InputGroupAddon>
            <InputGroupInput
              placeholder="Search name or email"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </InputGroup>
          <Select value={filter} onValueChange={(v) => setFilter(v as TierFilter)}>
            <SelectTrigger className="w-32 border-border">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All</SelectItem>
              <SelectItem value="free">Free</SelectItem>
              <SelectItem value="standard">Standard</SelectItem>
              <SelectItem value="pro">Pro</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      {usersQuery.isError && (
        <Alert variant="destructive">
          <AlertDescription>
            Failed to load users. Ensure api_server is running and ADMIN_API_KEY matches on both
            sides.
          </AlertDescription>
        </Alert>
      )}

      <div className="rounded-xl border border-border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>User</TableHead>
              <TableHead className="text-center">Tier</TableHead>
              <TableHead className="text-center">Created</TableHead>
              <TableHead className="text-center">Action</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {usersQuery.isLoading ? (
              [...Array(5)].map((_, i) => (
                <TableRow key={i}>
                  <TableCell colSpan={4}>
                    <Skeleton className="h-8 w-full" />
                  </TableCell>
                </TableRow>
              ))
            ) : pageRows.length === 0 ? (
              <TableRow>
                <TableCell colSpan={4} className="py-8 text-center text-sm text-muted-foreground">
                  {isFiltering ? "No users match." : "No users."}
                </TableCell>
              </TableRow>
            ) : (
              pageRows.map((u) => {
                const rowPending = mutation.isPending && pending?.user.id === u.id;
                // Every settable tier except the user's current one — the admin
                // can jump straight to any of them (e.g. free → pro directly).
                const options = TIER_LADDER.filter((t) => t !== u.tier);
                return (
                  <TableRow key={u.id}>
                    <TableCell>
                      <div className="flex items-center gap-3">
                        <Avatar className="h-9 w-9">
                          <AvatarFallback className="bg-muted text-xs font-medium text-muted-foreground">
                            {initials(u)}
                          </AvatarFallback>
                        </Avatar>
                        <div className="min-w-0">
                          <div className="truncate text-sm font-medium">{displayName(u)}</div>
                          <div className="truncate text-xs text-muted-foreground">{u.email}</div>
                        </div>
                      </div>
                    </TableCell>
                    <TableCell className="text-center">
                      <TierBadge tier={u.tier} />
                    </TableCell>
                    <TableCell className="text-center text-sm text-muted-foreground">
                      {u.created_at ? format(parseISO(u.created_at), "MMM d, yyyy") : "—"}
                    </TableCell>
                    <TableCell className="text-center">
                      <div className="flex items-center justify-center gap-2">
                        <Button
                          variant="ghost"
                          size="sm"
                          className="gap-1.5"
                          onClick={() => setHistoryUser(u)}
                          title="Tier-change history"
                        >
                          <History className="h-3.5 w-3.5" />
                        </Button>
                        {/* Pick any other tier to move this user to; value stays
                            empty so the trigger reads as an action, not a bound
                            field. Choosing one opens the confirm dialog. */}
                        <Select
                          value=""
                          disabled={mutation.isPending}
                          onValueChange={(tier) => setPending({ user: u, tier })}
                        >
                          <SelectTrigger
                            className="w-36 border-border"
                            aria-label={`Change tier for ${u.email}`}
                          >
                            <SelectValue placeholder={rowPending ? "Updating…" : "Change tier"} />
                          </SelectTrigger>
                          <SelectContent>
                            {options.map((t) => (
                              <SelectItem key={t} value={t}>
                                {LABEL[t]}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>

      {showPagination && (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-muted-foreground">
            Showing {start + 1}–{Math.min(start + PAGE_SIZE, filtered.length)} of {filtered.length}
          </p>
          <Pagination className="mx-0 w-auto justify-end">
            <PaginationContent>
              <PaginationItem>
                <PaginationPrevious
                  aria-disabled={currentPage <= 1}
                  className={currentPage <= 1 ? "pointer-events-none opacity-50" : "cursor-pointer"}
                  onClick={() => setPage(Math.max(1, currentPage - 1))}
                />
              </PaginationItem>
              <PaginationItem>
                <PaginationNext
                  aria-disabled={currentPage >= pageCount}
                  className={
                    currentPage >= pageCount ? "pointer-events-none opacity-50" : "cursor-pointer"
                  }
                  onClick={() => setPage(Math.min(pageCount, currentPage + 1))}
                />
              </PaginationItem>
            </PaginationContent>
          </Pagination>
        </div>
      )}

      <AlertDialog
        open={pending !== null}
        onOpenChange={(o) => !o && !mutation.isPending && setPending(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Change {pending?.user.email} to {pending ? (LABEL[pending.tier] ?? pending.tier) : ""}
              ?
            </AlertDialogTitle>
            <AlertDialogDescription>
              This overrides the user's plan immediately.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={mutation.isPending}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={mutation.isPending}
              onClick={(e) => {
                e.preventDefault();
                if (pending) mutation.mutate({ userId: pending.user.id, tier: pending.tier });
              }}
            >
              Confirm
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={historyUser !== null} onOpenChange={(o) => !o && setHistoryUser(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Tier history — {historyUser?.email}</DialogTitle>
            <DialogDescription>Who changed this user's tier, and when.</DialogDescription>
          </DialogHeader>
          {historyQuery.isLoading ? (
            <p className="py-4 text-sm text-muted-foreground">Loading…</p>
          ) : historyQuery.isError ? (
            <p className="py-4 text-sm text-destructive">Couldn't load tier history.</p>
          ) : (historyQuery.data?.length ?? 0) === 0 ? (
            <p className="py-4 text-sm text-muted-foreground">No tier changes recorded.</p>
          ) : (
            <div className="max-h-80 overflow-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>When</TableHead>
                    <TableHead>Change</TableHead>
                    <TableHead>By</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {historyQuery.data!.map((e, i) => (
                    <TableRow key={i}>
                      <TableCell className="whitespace-nowrap text-sm text-muted-foreground">
                        {format(parseISO(e.created_at), "MMM d, yyyy HH:mm")}
                      </TableCell>
                      <TableCell className="text-sm">
                        {LABEL[e.old_tier] ?? e.old_tier} → {LABEL[e.new_tier] ?? e.new_tier}
                      </TableCell>
                      <TableCell className="text-sm">{e.actor_email}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
