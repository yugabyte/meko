import { createRoot } from "react-dom/client";
import App from "./app/App";
import { initThemeBeforeMount } from "./lib/theme";
import { loadTelemetry } from "./lib/telemetry/rudderstack";
import { captureAttribution } from "./lib/telemetry/attribution";
import "./index.css";

// Apply the persisted theme (or OS preference) BEFORE React mounts so
// the first paint already shows the right palette. Without this, users
// on the non-default mode see a brief flash of the wrong theme on every
// load. The hook handles steady-state changes after this.
initThemeBeforeMount();

// Capture ad-campaign attribution from the landing URL BEFORE any OAuth redirect
// can strip the query string; persisted first-touch and attached to signup identify.
captureAttribution();

// Boot RudderStack once before mount. No-ops when no write key is configured.
loadTelemetry();

createRoot(document.getElementById("root")!).render(<App />);
