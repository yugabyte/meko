// The web UI's entry: renders the gate, which shows the app once Meko says how it takes this browser.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Gate } from "./Gate";
import { initialTheme } from "./lib/theme";
import "./index.css";

// The gate's screens come before the app, which keeps the theme from then on.
document.documentElement.classList.toggle("dark", initialTheme() === "dark");

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Gate />
  </StrictMode>,
);
