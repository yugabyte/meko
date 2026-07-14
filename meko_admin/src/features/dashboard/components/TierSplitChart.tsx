import { PieChart, Pie, Cell, Tooltip, ResponsiveContainer, Legend } from "recharts";
import { CreditCard } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

type Props = {
  free: number | undefined;
  pro: number | undefined;
  isLoading: boolean;
};

const COLORS = ["hsl(217 91% 60%)", "hsl(142 71% 45%)"]; // free, pro

// Free vs Pro tier split (org-wide). Donut so the ratio reads at a glance.
export function TierSplitChart({ free, pro, isLoading }: Props) {
  const data = [
    { name: "Free", value: free ?? 0 },
    { name: "Pro", value: pro ?? 0 },
  ];
  const empty = (free ?? 0) + (pro ?? 0) === 0;

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium">Users by tier</CardTitle>
        <CreditCard className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-48 w-full" />
        ) : empty ? (
          <div className="flex h-48 items-center justify-center text-sm text-muted-foreground">
            No users yet
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={200}>
            <PieChart>
              <Pie data={data} dataKey="value" nameKey="name" innerRadius={50} outerRadius={80} paddingAngle={2}>
                {data.map((_, i) => (
                  <Cell key={i} fill={COLORS[i]} />
                ))}
              </Pie>
              <Legend iconType="circle" />
              <Tooltip
                contentStyle={{
                  backgroundColor: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "6px",
                  fontSize: "12px",
                }}
              />
            </PieChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  );
}
