// SavingsLeaderboard: top datapack owners by tokens saved. Each row shows an
// inline efficiency bar (% context saved), the user, their request count, and
// dollars saved vs the top premier model.
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { ExecUserSavings } from "@/lib/adminApi";
import { dollarsSavedVsTopModel, formatUsd, tokenBurnPercent } from "@/lib/tokenSavings";

type Props = {
  users: ExecUserSavings[] | undefined;
  isLoading?: boolean;
};

function displayName(u: ExecUserSavings): string {
  const name = `${u.first_name ?? ""} ${u.last_name ?? ""}`.trim();
  return name || u.email;
}

// Green→amber→orange by how much context was trimmed, so a glance reads adoption.
function effColor(pct: number): string {
  if (pct >= 60) return "bg-emerald-500";
  if (pct >= 30) return "bg-amber-500";
  return "bg-orange-500";
}

export function SavingsLeaderboard({ users, isLoading }: Props) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Top savers</CardTitle>
        <CardDescription>
          Datapack owners with the most tokens saved (min 20 contextual searches).
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-44 text-center">Efficiency</TableHead>
              <TableHead className="text-center">User</TableHead>
              <TableHead className="text-center">Requests</TableHead>
              <TableHead className="text-center">Saved</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              [...Array(5)].map((_, i) => (
                <TableRow key={i}>
                  <TableCell colSpan={4}>
                    <Skeleton className="h-8 w-full" />
                  </TableCell>
                </TableRow>
              ))
            ) : !users || users.length === 0 ? (
              <TableRow>
                <TableCell colSpan={4} className="py-8 text-center text-sm text-muted-foreground">
                  No users have reached 20 contextual searches yet.
                </TableCell>
              </TableRow>
            ) : (
              users.map((u) => {
                const pct = tokenBurnPercent(u.request_count, u.tokens_saved);
                const dollars = dollarsSavedVsTopModel(u.request_count, u.tokens_saved);
                return (
                  <TableRow key={u.user_id}>
                    <TableCell>
                      <div className="flex items-center justify-center gap-2">
                        <div className="h-2 w-24 overflow-hidden rounded-full bg-muted">
                          <div
                            className={`h-full rounded-full ${effColor(pct)}`}
                            style={{ width: `${pct}%` }}
                          />
                        </div>
                        <span className="w-9 text-right text-sm font-medium tabular-nums">
                          {pct}%
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-center">
                      <div className="text-sm font-medium">{displayName(u)}</div>
                      <div className="text-xs text-muted-foreground">{u.email}</div>
                    </TableCell>
                    <TableCell className="text-center text-sm tabular-nums">
                      {u.request_count.toLocaleString()}
                    </TableCell>
                    <TableCell className="text-center text-sm font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">
                      {formatUsd(dollars)}
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
