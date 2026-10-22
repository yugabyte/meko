// The web UI's entry: redeems the one-time code meko ui opened the page with, then renders the app.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { adoptCodeFromURL } from "./lib/token";
import "./index.css";

void adoptCodeFromURL().then(() =>
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  ),
);
