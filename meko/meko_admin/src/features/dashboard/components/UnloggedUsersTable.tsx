import { format, parseISO } from "date-fns";
import { UserX } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { type CognitoUser } from "@/lib/adminApi";

type Props = {
  users: CognitoUser[] | undefined;
  isLoading: boolean;
};

const STATUS_LABELS: Record<string, { label: string; variant: "secondary" | "outline" }> = {
  FORCE_CHANGE_PASSWORD: { label: "Never signed in", variant: "secondary" },
  UNCONFIRMED: { label: "Unconfirmed", variant: "outline" },
};

export function UnloggedUsersTable({ users, isLoading }: Props) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle className="text-sm font-medium">
          Users with no login
        </CardTitle>
        {!isLoading && users !== undefined && (
          <span className="text-xs text-muted-foreground">{users.length} user{users.length !== 1 ? "s" : ""}</span>
        )}
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="space-y-2">
            {[...Array(4)].map((_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : !users || users.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-8 text-muted-foreground gap-2">
            <UserX className="h-8 w-8 opacity-40" />
            <p className="text-sm">All registered users have signed in</p>
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Email</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Registered</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.map((u, idx) => {
                const meta = STATUS_LABELS[u.status] ?? {
                  label: u.status,
                  variant: "outline" as const,
                };
                return (
                  <TableRow key={idx}>
                    <TableCell className="text-sm font-medium">{u.email}</TableCell>
                    <TableCell>
                      <Badge variant={meta.variant}>{meta.label}</Badge>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {u.createdAt
                        ? format(parseISO(u.createdAt), "MMM d, yyyy")
                        : "—"}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
