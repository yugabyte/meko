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

/** Default Yugabyte data plane, mirroring the snippet used by yugabyte-cloud. Overridable via env. */
const DEFAULT_DATA_PLANE_URL = "https://yugabyteklhgka.dataplane.rudderstack.com";

const WRITE_KEY = (import.meta.env.VITE_RUDDERSTACK_WRITE_KEY || "").trim();

const DATA_PLANE_URL = (
  import.meta.env.VITE_RUDDERSTACK_DATA_PLANE_URL || DEFAULT_DATA_PLANE_URL
).trim();

/** The single SDK instance. `null` until loadTelemetry() runs with a valid write key. */
let analytics: RudderAnalytics | null = null;

/** Identity traits forwarded to destinations (HubSpot maps these to contact fields). */
export type TelemetryUser = {
  id: string;
  email: string;
  firstName?: string;
  lastName?: string;
  /** Billing tier, when known. */
  userType?: string;
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

/** Track a named event with optional properties. */
export function trackEvent(event: TelemetryEventName, properties: ApiObject = {}): void {
  analytics?.track(event, properties);
}

/** Associate the current browser with a known user, forwarding identity traits. */
export function identifyUser(user: TelemetryUser): void {
  if (!analytics) return;
  analytics.identify(user.id, {
    email: user.email,
    firstName: user.firstName,
    lastName: user.lastName,
    userType: user.userType,
  });
}

/** Record a page view. Called on every route change by <TrackPageViews />. */
export function trackPageView(path: string): void {
  analytics?.page(path, { path });
}

/** Clear the identified user — call on logout so events aren't attributed across sessions. */
export function resetTelemetry(): void {
  analytics?.reset();
}
