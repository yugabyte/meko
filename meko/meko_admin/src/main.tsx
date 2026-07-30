import { createRoot } from "react-dom/client";
import App from "./app/App";
import { initThemeBeforeMount } from "./lib/theme";
import "./index.css";

initThemeBeforeMount();

createRoot(document.getElementById("root")!).render(<App />);
