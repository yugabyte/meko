// Share page card for the datapack's one share link: turn it on or off, pick the
// role it gives, copy the URL, and see how many people joined through it.
import { useCallback, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Copy, Link2, Loader2, RotateCcw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "@/hooks/use-toast";
import { useArrivalFlash } from "@/hooks/useArrivalFlash";
import { cn } from "@/lib/utils";
import { shareLinkPath } from "@/app/routes/paths";
import { getApiErrorMessage, isMekoApiRequestError } from "@/lib/api/mekoApi";
import {
  SHARE_LINK_GRANTS,
  SHARE_LINK_STATUS_ACTIVE,
  SHARE_LINK_STATUS_OFF,
  type ShareLinkGrant,
} from "@/lib/api/shareLinksApi";
import { formatDatapackShareGrantCamelCase } from "@/lib/datapackShareGrants";
import {
  SHARE_PAGE_HIGHLIGHT_LINK,
  SHARE_PAGE_HIGHLIGHT_PARAM,
  shareLinkUrl,
} from "@/lib/linkSharingFeature";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";
import { ResetShareLinkDialog } from "@/features/datapacks/components/datapack-share/ResetShareLinkDialog";
import {
  copyShareLinkUrl,
  useDatapackShareLink,
  useUpdateDatapackShareLink,
} from "@/features/datapacks/hooks/useDatapackShareLink";

type Props = {
  datapackId: string;
  /** Called after a role change, which moves the members who joined by link. */
  onMembersMoved?: () => void;
};

export function ShareLinkSection({ datapackId, onMembersMoved }: Props) {
  const link = useDatapackShareLink(datapackId);
  const update = useUpdateDatapackShareLink(datapackId);

  // ?highlight=link (from the details page's link button): scroll here and
  // flash once the card has loaded, then drop the param so a reload doesn't.
  const [searchParams, setSearchParams] = useSearchParams();
  const highlight = searchParams.get(SHARE_PAGE_HIGHLIGHT_PARAM) === SHARE_PAGE_HIGHLIGHT_LINK;
  const clearHighlight = useCallback(
    () =>
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          next.delete(SHARE_PAGE_HIGHLIGHT_PARAM);
          return next;
        },
        { replace: true },
      ),
    [setSearchParams],
  );
  const { ref, flashing } = useArrivalFlash<HTMLDivElement>({
    flashKey: highlight ? SHARE_PAGE_HIGHLIGHT_LINK : undefined,
    active: highlight && link.isSuccess,
    onDone: clearHighlight,
    debugLabel: "ShareLinkSection",
  });

  const [confirmReset, setConfirmReset] = useState(false);

  // Viewers and contributors can open this page; the API refuses them the link
  // with 403. Any other failure is shown with a retry, since the query doesn't retry.
  if (link.isError && isMekoApiRequestError(link.error) && link.error.httpStatus === 403) {
    return null;
  }

  const active = link.data?.status === SHARE_LINK_STATUS_ACTIVE;
  const grant: ShareLinkGrant = link.data?.grant ?? "viewer";
  const token = link.data?.token;
  const movedCount = link.data?.members_at_link_role ?? 0;
  const change = (body: Parameters<typeof update.mutate>[0]) =>
    update.mutate(body, {
      onSuccess: () => {
        if (body.grant) onMembersMoved?.();
        if (body.reset) {
          setConfirmReset(false);
          toast({ title: "Link reset", description: "The old link no longer works." });
        }
      },
      onError: (err) =>
        toast({
          title: "Couldn't update the link",
          description: getApiErrorMessage(err, ""),
          variant: "destructive",
        }),
    });

  return (
    <div
      ref={ref}
      className={cn(
        "scroll-mt-6 rounded-xl transition-shadow",
        flashing && "ring-4 ring-brand/70 shadow-lg animate-pulse",
      )}
    >
      <DatapackShareSectionCard
        title="Link sharing"
        icon={<Link2 aria-hidden />}
        trailing={
          link.data ? (
            <span className="text-xs text-muted-foreground tabular-nums">
              {link.data.redemption_count} joined via link
            </span>
          ) : null
        }
      >
        {link.isPending ? (
          <Loader2
            className="h-5 w-5 animate-spin text-muted-foreground"
            aria-label="Loading link"
          />
        ) : link.isError ? (
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-sm text-muted-foreground">
              {getApiErrorMessage(link.error, "Couldn't load the link.")}
            </p>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={link.isFetching}
              onClick={() => void link.refetch()}
            >
              Retry
            </Button>
          </div>
        ) : (
          <div className="space-y-4">
            <div className="flex items-start justify-between gap-4">
              <div className="min-w-0 space-y-1">
                <Label htmlFor="share-link-active" className="text-sm font-medium text-foreground">
                  Anyone with the link can join
                </Label>
                <p className="text-xs leading-relaxed text-muted-foreground">
                  They sign in or sign up, then join without an invite. Turning the link off stops
                  new joins; people who already joined keep access.
                </p>
              </div>
              <Switch
                id="share-link-active"
                checked={active}
                disabled={update.isPending}
                onCheckedChange={(on) =>
                  change({ status: on ? SHARE_LINK_STATUS_ACTIVE : SHARE_LINK_STATUS_OFF })
                }
              />
            </div>

            {active && token ? (
              <div className="space-y-2">
                <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                  <Select
                    value={grant}
                    disabled={update.isPending}
                    onValueChange={(g) => change({ grant: g as ShareLinkGrant })}
                  >
                    <SelectTrigger className="sm:w-40" aria-label="Role the link gives">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {SHARE_LINK_GRANTS.map((g) => (
                        <SelectItem key={g} value={g}>
                          {formatDatapackShareGrantCamelCase(g)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <Input
                    readOnly
                    aria-label="Share link"
                    value={shareLinkUrl(shareLinkPath(token))}
                    className="font-mono text-xs"
                    onFocus={(e) => e.currentTarget.select()}
                  />
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => void copyShareLinkUrl(token)}
                  >
                    <Copy className="mr-1.5 h-4 w-4" aria-hidden />
                    Copy link
                  </Button>
                </div>
                {movedCount > 0 ? (
                  <p className="text-xs text-muted-foreground">
                    Changing the role also changes it for the {movedCount}{" "}
                    {movedCount === 1 ? "person" : "people"} who joined through this link.
                  </p>
                ) : null}
              </div>
            ) : null}

            {/* Shown on or off, so a leaked link can be reset before it is turned back on. */}
            {token ? (
              <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3">
                <p className="text-xs text-muted-foreground">
                  Shared the link somewhere it shouldn&apos;t be? Reset it to make a new one.
                </p>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={update.isPending}
                  onClick={() => setConfirmReset(true)}
                >
                  <RotateCcw className="mr-1.5 h-3.5 w-3.5" aria-hidden />
                  Reset link
                </Button>
              </div>
            ) : null}
            <ResetShareLinkDialog
              open={confirmReset}
              resetting={update.isPending}
              onCancel={() => setConfirmReset(false)}
              onConfirm={() => change({ reset: true })}
            />
          </div>
        )}
      </DatapackShareSectionCard>
    </div>
  );
}
