// Fires a RudderStack page() event on every route change. Renders nothing; mount it
// once inside <BrowserRouter> so it has router context (see App.tsx).
import { useEffect } from "react";
import { useLocation } from "react-router-dom";
import { trackPageView } from "@/lib/telemetry/rudderstack";

export function TrackPageViews(): null {
  const location = useLocation();

  useEffect(() => {
    trackPageView(location.pathname);
  }, [location.pathname]);

  return null;
}
