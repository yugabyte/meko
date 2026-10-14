// Signed-in side of /share/:token: joins the datapack behind the link once,
// then opens it. A link that is unknown or off says so instead.
import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { DATAPACKS_QUERY_KEY } from "@/features/datapacks/hooks/useDatapacks";
import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { toast } from "@/hooks/use-toast";
import { ROUTES, datapackDetailPath } from "@/app/routes/paths";
import { isMekoApiRequestError, getApiErrorMessage } from "@/lib/api/mekoApi";
import { SHARE_LINK_REMOVED_ERROR_CODE, redeemShareLink } from "@/lib/api/shareLinksApi";
import { clearPendingShareToken } from "@/lib/pendingShareLink";
import { formatDatapackShareGrantCamelCase } from "@/lib/datapackShareGrants";
import { notifyDatapacksListChanged } from "@/features/observe/observeRefresh";
import {
  SHARE_LINK_ALREADY_MEMBER_TITLE,
  SHARE_LINK_REMOVED_BODY,
  SHARE_LINK_REMOVED_TITLE,
  SHARE_LINK_UNAVAILABLE_BODY,
  SHARE_LINK_UNAVAILABLE_TITLE,
} from "../shareLinkCopy";

type Props = { token: string };

export default function ShareLinkRedeemPage({ token }: Props) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState<{ title: string; body: string } | null>(null);
  // Strict mode mounts effects twice; redeeming is idempotent, but one call is enough.
  const started = useRef(false);

  useEffect(() => {
    if (started.current) return;
    started.current = true;
    redeemShareLink(token)
      .then((res) => {
        clearPendingShareToken();
        notifyDatapacksListChanged(`share-link-redeemed:${res.datapack_id}`);
        void queryClient.invalidateQueries({ queryKey: [DATAPACKS_QUERY_KEY] });
        const role = formatDatapackShareGrantCamelCase(res.grant).toLowerCase();
        toast(
          res.already_member
            ? { title: SHARE_LINK_ALREADY_MEMBER_TITLE, description: `You have ${role} access.` }
            : { title: `You joined as ${role}` },
        );
        navigate(datapackDetailPath(res.datapack_id), { replace: true });
      })
      .catch((err) => {
        // A dead link, or one the owner removed you from, won't come back; a
        // network or server error might, so keep the pending link for the next attempt.
        // Matched on the code, not the 403, so a proxy's 403 doesn't say "removed".
        const status = isMekoApiRequestError(err) ? err.httpStatus : undefined;
        const removed = isMekoApiRequestError(err) && err.code === SHARE_LINK_REMOVED_ERROR_CODE;
        if (status === 404 || removed) clearPendingShareToken();
        setError(
          removed
            ? { title: SHARE_LINK_REMOVED_TITLE, body: SHARE_LINK_REMOVED_BODY }
            : {
                title: SHARE_LINK_UNAVAILABLE_TITLE,
                body:
                  status === 404
                    ? SHARE_LINK_UNAVAILABLE_BODY
                    : getApiErrorMessage(err, "Couldn't open the shared datapack."),
              },
        );
      });
  }, [navigate, queryClient, token]);

  return (
    <div className="flex min-h-screen flex-1 items-center justify-center bg-background p-6 text-foreground">
      {error ? (
        <div className="max-w-md space-y-3 text-center">
          <h1 className="text-xl font-semibold">{error.title}</h1>
          <p className="text-sm text-muted-foreground">{error.body}</p>
          <Button asChild variant="outline">
            <Link to={ROUTES.datapacks}>Go to your datapacks</Link>
          </Button>
        </div>
      ) : (
        <Loader2
          className="h-6 w-6 animate-spin text-muted-foreground"
          aria-label="Opening the shared datapack"
        />
      )}
    </div>
  );
}
