// ConversationsByAgentChart — horizontal bar chart of Langfuse conversations
// grouped by agent, with an empty/disabled state when Langfuse isn't configured.
import {
  BarChart,
  Bar,
  Cell,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import { MessageSquare } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

type Props = {
  data: { agent: string; conversations: number }[] | undefined;
  enabled: boolean | undefined;
  isLoading: boolean;
};

// Cycled per-bar palette so the chart reads as multi-color, echoing the metric
// tile accents (blue/orange/violet/emerald/pink/cyan/amber/sky).
const BAR_COLORS = [
  "hsl(217 91% 60%)",
  "hsl(25 95% 53%)",
  "hsl(258 90% 66%)",
  "hsl(160 84% 39%)",
  "hsl(330 81% 60%)",
  "hsl(189 94% 43%)",
  "hsl(43 96% 56%)",
  "hsl(199 89% 48%)",
];

export function ConversationsByAgentChart({ data, enabled, isLoading }: Props) {
  const rows = data ?? [];

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium">Conversations by agent</CardTitle>
        <MessageSquare className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-48 w-full" />
        ) : enabled === false ? (
          <div className="flex h-48 items-center justify-center text-sm text-muted-foreground">
            Langfuse not configured
          </div>
        ) : rows.length === 0 ? (
          <div className="flex h-48 items-center justify-center text-sm text-muted-foreground">
            No conversations yet
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={Math.max(200, rows.length * 40)}>
            <BarChart data={rows} layout="vertical" margin={{ top: 4, right: 16, left: 8, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" className="stroke-border" horizontal={false} />
              <XAxis
                type="number"
                allowDecimals={false}
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
              />
              <YAxis
                type="category"
                dataKey="agent"
                width={120}
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
              />
              <Tooltip
                contentStyle={{
                  backgroundColor: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "6px",
                  fontSize: "12px",
                }}
                cursor={{ fill: "hsl(var(--accent))" }}
              />
              <Bar dataKey="conversations" name="Conversations" radius={[0, 3, 3, 0]}>
                {rows.map((_, i) => (
                  <Cell key={i} fill={BAR_COLORS[i % BAR_COLORS.length]} />
                ))}
              </Bar>
            </BarChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  );
}
