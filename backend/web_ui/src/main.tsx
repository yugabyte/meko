// The web UI's entry: renders the sign-in gate, which shows the app once Meko lets this browser in.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Gate } from "./Gate";
import { initialTheme } from "./lib/theme";
import "./index.css";

// The sign-in screens come before the app, which keeps the theme from then on.
document.documentElement.classList.toggle("dark", initialTheme() === "dark");

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Gate />
  </StrictMode>,
);
