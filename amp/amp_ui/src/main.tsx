import { createRoot } from "react-dom/client";
import App from "./App";
import { initThemeBeforeMount } from "./lib/theme";
import "./index.css";

// Apply the persisted theme (or OS preference) BEFORE React mounts so the
// first paint already shows the right palette — avoids a flash of the wrong
// theme on load.
initThemeBeforeMount();

createRoot(document.getElementById("root")!).render(<App />);
