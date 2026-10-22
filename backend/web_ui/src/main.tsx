// The web UI's entry: adopts the token meko ui opened the page with, then renders the app.

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { adoptTokenFromURL } from "./lib/token";
import "./index.css";

adoptTokenFromURL();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
