import {
  CognitoIdentityProviderClient,
  ListUsersCommand,
  type UserType,
} from "@aws-sdk/client-cognito-identity-provider";
import { format, subDays } from "date-fns";

const client = new CognitoIdentityProviderClient({
  region: process.env.AWS_REGION ?? "us-east-1",
});

const USER_POOL_ID = process.env.COGNITO_USER_POOL_ID ?? "";

export type CognitoUser = {
  email: string;
  status: string;
  createdAt: string;
};

export type DailyLoginPoint = {
  date: string;
  count: number;
};

function extractEmail(user: UserType): string {
  const attr = user.Attributes?.find((a) => a.Name === "email");
  return attr?.Value ?? user.Username ?? "unknown";
}

/** Paginates through the full user pool and calls fn on each page. */
async function paginateUsers(fn: (users: UserType[]) => void, filter?: string) {
  let paginationToken: string | undefined;
  do {
    const cmd = new ListUsersCommand({
      UserPoolId: USER_POOL_ID,
      Filter: filter,
      Limit: 60,
      PaginationToken: paginationToken,
    });
    const resp = await client.send(cmd);
    fn(resp.Users ?? []);
    paginationToken = resp.PaginationToken;
  } while (paginationToken);
}

/**
 * Returns users whose accounts were created but who have never signed in.
 * FORCE_CHANGE_PASSWORD = admin-provisioned, pending first login.
 * UNCONFIRMED           = self-registered but email not yet verified.
 * Uses `cognito:user_status` — the correct filter attribute for account status.
 */
export async function getNeverLoggedInUsers(): Promise<CognitoUser[]> {
  if (!USER_POOL_ID) {
    console.warn("COGNITO_USER_POOL_ID is not set — skipping Cognito query");
    return [];
  }

  const results: CognitoUser[] = [];

  for (const status of ["FORCE_CHANGE_PASSWORD", "UNCONFIRMED"] as const) {
    await paginateUsers((users) => {
      for (const u of users) {
        results.push({
          email: extractEmail(u),
          status,
          createdAt: u.UserCreateDate?.toISOString() ?? "",
        });
      }
    }, `cognito:user_status = "${status}"`);
  }

  return results;
}

/**
 * Returns daily login counts for the last N days by reading UserLastModifiedDate
 * from every user in the pool. Cognito updates this timestamp on each auth event,
 * so it serves as a reliable login-activity proxy without needing Advanced Security.
 */
export async function getDailyLogins(days = 7): Promise<DailyLoginPoint[]> {
  const zeroDays: DailyLoginPoint[] = Array.from({ length: days }, (_, i) => ({
    date: format(subDays(new Date(), days - 1 - i), "yyyy-MM-dd"),
    count: 0,
  }));

  if (!USER_POOL_ID) {
    console.warn("COGNITO_USER_POOL_ID is not set — returning zero login data");
    return zeroDays;
  }

  const cutoff = subDays(new Date(), days);
  const countByDate: Record<string, number> = {};

  await paginateUsers((users) => {
    for (const u of users) {
      if (u.UserLastModifiedDate && u.UserLastModifiedDate > cutoff) {
        const dateKey = format(u.UserLastModifiedDate, "yyyy-MM-dd");
        countByDate[dateKey] = (countByDate[dateKey] ?? 0) + 1;
      }
    }
  });

  return zeroDays.map((d) => ({ ...d, count: countByDate[d.date] ?? 0 }));
}
