import { createRoot } from "react-dom/client";
import App from "./app/App";
import { initThemeBeforeMount } from "./lib/theme";
import "./index.css";

// Apply the persisted theme (or OS preference) BEFORE React mounts so
// the first paint already shows the right palette. Without this, users
// on the non-default mode see a brief flash of the wrong theme on every
// load. The hook handles steady-state changes after this.
initThemeBeforeMount();

createRoot(document.getElementById("root")!).render(<App />);
