import {
  BarChart,
  Bar,
  Cell,
  LabelList,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import { MessageSquare } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { BAR_COLORS } from "./chartColors";

type Props = {
  data: { agent: string; conversations: number }[] | undefined;
  enabled: boolean | undefined;
  isLoading: boolean;
};

// Per-bar height and the fixed viewport the bars scroll within. A heavy user /
// deployment can have dozens of agents; capping the viewport keeps the card a
// constant size (the list scrolls) instead of stretching down the page.
const ROW_HEIGHT = 36;
const MIN_CHART_HEIGHT = 200;
const MAX_VIEWPORT_HEIGHT = 340;

// Conversations grouped by agent (Langfuse). Horizontal bars so agent names
// stay readable regardless of count; the bar area scrolls when there are many.
export function ConversationsByAgentChart({ data, enabled, isLoading }: Props) {
  const rows = data ?? [];
  const chartHeight = Math.max(MIN_CHART_HEIGHT, rows.length * ROW_HEIGHT);

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
          <div className="overflow-y-auto" style={{ maxHeight: MAX_VIEWPORT_HEIGHT }}>
            <ResponsiveContainer width="100%" height={chartHeight}>
              <BarChart
                data={rows}
                layout="vertical"
                margin={{ top: 4, right: 40, left: 8, bottom: 0 }}
              >
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
                  <LabelList
                    dataKey="conversations"
                    position="right"
                    className="fill-foreground"
                    fontSize={11}
                  />
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
