// Inline notice shown when an AMP action (create cluster / add or resize
// compute) is blocked by the per-tier vCPU cap. Renders the backend message
// (which already names the tier to move to) plus a deeplink to settings.
//
// Uses a plain <a>, NOT a react-router <Link>: one render path is a toast
// description, and <Toaster/> is a sibling of <BrowserRouter> (App.tsx), so a
// <Link> here would call useHref outside a Router context and throw while
// rendering the toast. A full-navigation anchor to /settings works everywhere.
import { ROUTES } from "@/app/routes/paths";

export function CoreLimitNotice({ message }: { message: string }) {
  return (
    <span>
      {message}{" "}
      <a
        href={ROUTES.settings}
        className="font-medium underline underline-offset-2 hover:no-underline"
      >
        Go to settings
      </a>
    </span>
  );
}
