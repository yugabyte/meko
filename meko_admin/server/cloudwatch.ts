import {
  CloudWatchClient,
  GetMetricStatisticsCommand,
} from "@aws-sdk/client-cloudwatch";
import { format, subDays, startOfDay } from "date-fns";

const client = new CloudWatchClient({
  region: process.env.AWS_REGION ?? "us-east-1",
});

const USER_POOL_ID = process.env.COGNITO_USER_POOL_ID ?? "";

export type DailyLoginPoint = {
  date: string;
  count: number;
};

/** Returns daily SignInSuccesses for the last 7 days from CloudWatch.
 *  Falls back to zeros when Cognito credentials/pool are not configured. */
export async function getDailyLogins(days = 7): Promise<DailyLoginPoint[]> {
  const endTime = new Date();
  const startTime = subDays(startOfDay(endTime), days - 1);

  const zeroDays: DailyLoginPoint[] = Array.from({ length: days }, (_, i) => ({
    date: format(subDays(endTime, days - 1 - i), "yyyy-MM-dd"),
    count: 0,
  }));

  if (!USER_POOL_ID) {
    console.warn("COGNITO_USER_POOL_ID is not set — returning zero login data");
    return zeroDays;
  }

  try {
    const cmd = new GetMetricStatisticsCommand({
      Namespace: "AWS/Cognito",
      MetricName: "SignInSuccesses",
      Dimensions: [{ Name: "UserPool", Value: USER_POOL_ID }],
      StartTime: startTime,
      EndTime: endTime,
      Period: 86400,
      Statistics: ["Sum"],
    });

    const resp = await client.send(cmd);
    const byDate: Record<string, number> = {};
    for (const dp of resp.Datapoints ?? []) {
      if (dp.Timestamp) {
        byDate[format(dp.Timestamp, "yyyy-MM-dd")] = dp.Sum ?? 0;
      }
    }

    return zeroDays.map((d) => ({ ...d, count: byDate[d.date] ?? 0 }));
  } catch (err) {
    console.error("CloudWatch GetMetricStatistics error:", err);
    return zeroDays;
  }
}
