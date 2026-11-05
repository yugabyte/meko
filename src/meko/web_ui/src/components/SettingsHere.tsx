// A link to the Settings page, for a note saying a feature is off: "turn it on in Settings".

import { href, navigate, SETTINGS } from "../lib/route";

export function SettingsHere({ children = "Settings" }: { children?: string }) {
  return (
    <a
      href={href(SETTINGS)}
      onClick={(e) => (e.preventDefault(), navigate(SETTINGS))}
      className="font-medium text-primary hover:underline"
    >
      {children}
    </a>
  );
}
