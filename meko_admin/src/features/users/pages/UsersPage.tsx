// Admin Users page: lists all users and lets an admin flip each user's tier
// (Free <-> Pro), confirming the change through an AlertDialog first.
import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { format, parseISO } from "date-fns";
import { toast } from "sonner";
import { ArrowUpCircle, ArrowDownCircle, Loader2 } from "lucide-react";
import axios from "axios";
import { fetchAdminUsers, updateUserTier, type AdminUser } from "@/lib/adminApi";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertDescription } from "@/components/ui/alert";
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

type TierFilter = "all" | "free" | "pro";

const LABEL: Record<string, string> = { free: "Free", pro: "Pro" };

function errMessage(e: unknown, fallback: string): string {
  if (axios.isAxiosError(e)) {
    const m = (e.response?.data as { message?: string } | undefined)?.message;
    return m ?? e.message;
  }
  return e instanceof Error ? e.message : fallback;
}

export function UsersPage() {
  const [filter, setFilter] = useState<TierFilter>("all");
  // The user whose tier change is awaiting confirmation in the dialog.
  const [target, setTarget] = useState<AdminUser | null>(null);
  const queryClient = useQueryClient();

  const usersQuery = useQuery({
    queryKey: ["admin-users"],
    queryFn: fetchAdminUsers,
    staleTime: 60_000,
  });

  const mutation = useMutation({
    mutationFn: ({ userId, tier }: { userId: string; tier: "free" | "pro" }) =>
      updateUserTier(userId, tier),
    onSuccess: (_data, { tier }) => {
      toast.success(`Changed ${target?.email ?? "user"} to ${LABEL[tier]}`);
      queryClient.invalidateQueries({ queryKey: ["admin-users"] });
      queryClient.invalidateQueries({ queryKey: ["admin-usage"] });
    },
    onError: (e) => toast.error(errMessage(e, "Could not update tier.")),
    onSettled: () => setTarget(null),
  });

  const users = usersQuery.data;
  const filtered = useMemo(
    () => (users ?? []).filter((u) => (filter === "all" ? true : u.tier === filter)),
    [users, filter],
  );

  const targetTier: "free" | "pro" = target?.tier === "pro" ? "free" : "pro";

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Users</h1>
          <p className="text-sm text-muted-foreground mt-0.5">Manage user tiers.</p>
        </div>
        <Select value={filter} onValueChange={(v) => setFilter(v as TierFilter)}>
          <SelectTrigger className="w-32 border-border">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All</SelectItem>
            <SelectItem value="free">Free</SelectItem>
            <SelectItem value="pro">Pro</SelectItem>
          </SelectContent>
        </Select>
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
              <TableHead>Email</TableHead>
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
            ) : filtered.length === 0 ? (
              <TableRow>
                <TableCell colSpan={4} className="py-8 text-center text-sm text-muted-foreground">
                  No users.
                </TableCell>
              </TableRow>
            ) : (
              filtered.map((u) => {
                const isThisPending = mutation.isPending && target?.id === u.id;
                return (
                  <TableRow key={u.id}>
                    <TableCell className="text-sm font-medium">{u.email}</TableCell>
                    <TableCell className="text-center">
                      {u.tier === "pro" ? (
                        <Badge
                          variant="outline"
                          className="border-transparent bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
                        >
                          Pro
                        </Badge>
                      ) : (
                        <Badge variant="secondary">Free</Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-center text-sm text-muted-foreground">
                      {u.created_at ? format(parseISO(u.created_at), "MMM d, yyyy") : "—"}
                    </TableCell>
                    <TableCell className="text-center">
                      <Button
                        variant="ghost"
                        size="sm"
                        className={
                          u.tier === "pro"
                            ? "min-w-[7rem] gap-1.5 text-muted-foreground"
                            : "min-w-[7rem] gap-1.5 text-emerald-600 hover:bg-emerald-500/10 hover:text-emerald-600 dark:text-emerald-400"
                        }
                        disabled={mutation.isPending}
                        onClick={() => setTarget(u)}
                      >
                        {isThisPending ? (
                          <Loader2 className="h-3.5 w-3.5 animate-spin" />
                        ) : u.tier === "pro" ? (
                          <ArrowDownCircle className="h-3.5 w-3.5" />
                        ) : (
                          <ArrowUpCircle className="h-3.5 w-3.5" />
                        )}
                        {u.tier === "pro" ? "Free" : "Pro"}
                      </Button>
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>

      <AlertDialog open={target !== null} onOpenChange={(o) => !o && !mutation.isPending && setTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Change {target?.email} to {LABEL[targetTier]}?
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
                if (target) mutation.mutate({ userId: target.id, tier: targetTier });
              }}
            >
              Confirm
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
