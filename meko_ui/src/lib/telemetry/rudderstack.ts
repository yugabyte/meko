// RudderStack telemetry wrapper. One module owns the SDK instance; the rest of the
// app calls the typed helpers below (trackEvent / identifyUser / trackPageView /
// resetTelemetry) and never touches the SDK directly.
//
// Loading is driven by env vars (Vite `VITE_` prefix). When no write key is set —
// local dev, preview builds — every helper is a safe no-op so nothing breaks and no
// events are sent. Events ultimately fan out to configured RudderStack destinations
// (e.g. HubSpot) from the dashboard, not from here.
import { RudderAnalytics, type ApiObject } from "@rudderstack/analytics-js";
import type { TelemetryEventName } from "@/lib/telemetry/events";
import { readRuntimeEnv } from "@/lib/runtimeEnv";

/** Default Yugabyte data plane, mirroring the snippet used by yugabyte-cloud. Overridable via env. */
const DEFAULT_DATA_PLANE_URL = "https://yugabyteklhgka.dataplane.rudderstack.com";

// Read at runtime (window.__MEKO_RUNTIME__, served by the Express server from the
// pod's env) with a build-time import.meta.env fallback — so the same image works
// across environments and the write key comes from the K8s secret at deploy time.
const WRITE_KEY = (readRuntimeEnv("VITE_RUDDERSTACK_WRITE_KEY") || "").trim();

const DATA_PLANE_URL = (
  readRuntimeEnv("VITE_RUDDERSTACK_DATA_PLANE_URL") || DEFAULT_DATA_PLANE_URL
).trim();

/** The single SDK instance. `null` until loadTelemetry() runs with a valid write key. */
let analytics: RudderAnalytics | null = null;

/** Identity traits forwarded to destinations (HubSpot maps these to contact fields).
 *  `id` is optional: at signup we know the email but not the account_id yet, so we
 *  identify by email trait only (HubSpot keys contacts on email). */
export type TelemetryUser = {
  id?: string;
  email: string;
  firstName?: string;
  lastName?: string;
};

/**
 * Initialize RudderStack. Safe to call once at app start. No-ops (and logs a hint in
 * dev) when no write key is configured, so the app runs identically without analytics.
 */
export function loadTelemetry(): void {
  if (analytics) return; // already loaded
  if (!WRITE_KEY) {
    if (import.meta.env.DEV) {
      // eslint-disable-next-line no-console
      console.info("[telemetry] VITE_RUDDERSTACK_WRITE_KEY not set — analytics disabled.");
    }
    return;
  }
  try {
    const instance = new RudderAnalytics();
    instance.load(WRITE_KEY, DATA_PLANE_URL, {});
    analytics = instance;
  } catch (err) {
    console.error("[telemetry] Failed to initialize RudderStack:", err);
  }
}

/** Track a named event with optional properties. An optional callback runs once the
 *  SDK has captured the event — used at logout so reset() doesn't strip the event's
 *  traits before it's flushed. */
export function trackEvent(
  event: TelemetryEventName,
  properties: ApiObject = {},
  callback?: () => void,
): void {
  if (!analytics) {
    callback?.();
    return;
  }
  analytics.track(event, properties, callback);
}

/** Associate the current browser with a user so subsequent events carry
 *  context.traits (HubSpot maps them by email). Call on every authenticated boot,
 *  not just at sign-in, so no event goes out with empty traits. Identifies by
 *  account_id when known; otherwise by email trait alone (e.g. at signup). */
export function identifyUser(user: TelemetryUser): void {
  if (!analytics) return;
  const traits = {
    email: user.email,
    firstName: user.firstName,
    lastName: user.lastName,
  };
  if (user.id) {
    analytics.identify(user.id, traits);
  } else {
    analytics.identify(traits);
  }
}

/** Gap between the identify and the follow-up track on sign-in/sign-up, giving
 *  the identify time to land (and create/resolve the HubSpot contact) before the
 *  event that must map to it arrives. */
export const IDENTIFY_TRACK_GAP_MS = 5000;

/**
 * Identify the user, then fire a track event after IDENTIFY_TRACK_GAP_MS. Used at
 * sign-in / sign-up so the identify (contact creation in HubSpot) is processed
 * before the sign-in/sign-up event, which must attach to that contact.
 * No-ops entirely when telemetry is disabled.
 */
export function identifyThenTrack(
  user: TelemetryUser,
  event: TelemetryEventName,
  properties: ApiObject = {},
): void {
  if (!analytics) return;
  identifyUser(user);
  window.setTimeout(() => trackEvent(event, properties), IDENTIFY_TRACK_GAP_MS);
}

/** Record a page view. Called on every route change by <TrackPageViews />. */
export function trackPageView(path: string): void {
  analytics?.page(path, { path });
}

/** Clear the identified user — call on logout so events aren't attributed across sessions. */
export function resetTelemetry(): void {
  analytics?.reset();
}
