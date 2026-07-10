import axios from "axios";
import { adminHttp as http } from "./authContext";

export type DailyLoginPoint = {
  date: string;
  count: number;
};

export type CognitoUser = {
  email: string;
  status: string;
  createdAt: string;
};

export type AdminMetrics = {
  usage: {
    dailyLogins: DailyLoginPoint[];
    neverLoggedInUsers: CognitoUser[];
    totalUsers: number;
    userRegistrations: DailyLoginPoint[];
  };
  system: {
    queuedWorkItems: number;
    datapacksCreated: number;
    documentsProcessed: number;
    embeddingsGenerated: number;
  };
};

export async function fetchAdminMetrics(): Promise<AdminMetrics> {
  const { data } = await http.get<AdminMetrics>("/metrics");
  return data;
}

/* ---- AMP controller registration ---- */

export type AmpRegistration = { name: string; url: string; username?: string };

/** GET the current AMP registration, or null if AMP is not registered. */
export async function getAmpRegistration(): Promise<AmpRegistration | null> {
  try {
    const { data } = await http.get<AmpRegistration>("/amp-registration");
    return data;
  } catch (e) {
    if (axios.isAxiosError(e) && e.response?.status === 404) return null;
    throw e;
  }
}

/** POST to register (or replace) the AMP controller endpoint. */
export async function registerAmp(payload: {
  name: string;
  url: string;
  username?: string;
  password?: string;
}): Promise<AmpRegistration> {
  const { data } = await http.post<AmpRegistration>("/amp-registration", payload);
  return data;
}

/** PATCH a partial update of the registration. */
export async function updateAmpRegistration(
  patch: Partial<{ name: string; url: string; username: string; password: string }>,
): Promise<AmpRegistration> {
  const { data } = await http.patch<AmpRegistration>("/amp-registration", patch);
  return data;
}

/** DELETE the registration (unregister AMP). */
export async function unregisterAmp(): Promise<void> {
  await http.delete("/amp-registration");
}
