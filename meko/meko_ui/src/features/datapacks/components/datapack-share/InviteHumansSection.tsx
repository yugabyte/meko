// Unified "Invite humans" tab — replaces the split of "Invite a human · by email"
// and "Invite a group · by CSV" that landed in #779. The user types 1 address or
// pastes/uploads a list; the submit button routes accordingly:
//
//   count 1 → POST /datapacks/:id/share       (existing single-invite path,
//                                              inline field error, DATAPACK_SHARED telemetry)
//   count ≥2 → POST /datapacks/:id/bulk-share  (existing bulk path, per-row
//                                              results table, no per-address
//                                              telemetry fan-out — one rolled-up
//                                              event covers the batch)
//
// The tab itself no longer knows about "single vs bulk"; the user just types.
// The role tiles (Viewer / Contributor / Maintainer) are the sole role control —
// no redundant dropdown next to the button — and they double as documentation
// for what each role can do.
//
// If the bulk endpoint is not deployed on this server yet, the tab falls back
// to the same amber "not available" notice the old BulkInviteByCsvSection
// used — but only when the user was trying to send N ≥ 2 addresses; a single
// invite still works because /share has been on main forever.

import { useMemo, useState } from "react";
import { WarningBanner } from "@/components/ui/warning-banner";
import { Button } from "@/components/ui/button";
import { toast } from "@/hooks/use-toast";
import { ToastAction } from "@/components/ui/toast";
import { scrollToShareMembers } from "@/features/datapacks/components/datapack-share/shareMembersAnchor";
import {
  DATAPACK_BULK_SHARE_MAX_EMAILS,
  bulkShareDatapack,
  getApiErrorMessage,
  isMekoApiRequestError,
  inviteDatapackShare,
  isApiEndpointMissingError,
  type DatapackBulkShareResult,
} from "@/lib/api/mekoApi";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  type DatapackShareGrant,
} from "@/lib/datapackShareGrants";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { trackEvent } from "@/lib/telemetry/rudderstack";
import { nowIso } from "@/lib/telemetry/helpers";
import { type ShareInviteRoleId } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import {
  parseBulkShareEmails,
  summarizeBulkShareResults,
} from "@/features/datapacks/components/datapack-share/bulkShareEmails";
import { BulkInviteEmailInput } from "@/features/datapacks/components/datapack-share/BulkInviteEmailInput";
import { BulkInviteResultsPanel } from "@/features/datapacks/components/datapack-share/BulkInviteResultsPanel";
import { InviteRoleTiles } from "@/features/datapacks/components/datapack-share/InviteRoleTiles";

type Props = {
  datapackId: string;
  /** Email of the signed-in user. Dropped from the bulk payload here
   *  because the backend only reports `is_owner` for the datapack
   *  owner, not for the caller — so for a maintainer (who this page
   *  also admits, per `isDatapackGrantEligibleToShare`) this client-side
   *  filter is the only thing keeping their own address out of an
   *  invite. Also blocks the single-invite self-address case. */
  currentUserEmail?: string;
  /** The signed-in sharer's role on this datapack (telemetry shared_by_role). */
  sharedByRole?: DatapackShareGrant | null;
  /** Existing shares before this invite (members excl. owner + pending), for is_first_share / total_shares. */
  priorShareCount?: number;
  /** Refresh members + pending invites after a batch that changed anything.
   *  Called with `allClean=true` only when every address resulted in a new
   *  invite — i.e. nothing failed, nothing was already a member. Callers can
   *  use this to close a dialog on a successful batch and stay open on a
   *  partial failure so the user can read the per-row results panel. */
  onInvitesSent: (allClean: boolean) => void | Promise<void>;
  /** Where the "Go to Members" toast action should take the user when a row
   *  was already a member. When this card is rendered outside the Share page
   *  (e.g. inside InvitePeopleDialog on datapack-detail) the default scroll
   *  no-ops because the members anchor only lives on the Share page. */
  onGoToMembers?: () => void;
};

export function InviteHumansSection({
  datapackId,
  currentUserEmail,
  sharedByRole,
  priorShareCount = 0,
  onInvitesSent,
  onGoToMembers,
}: Props) {
  const goToMembers = onGoToMembers ?? scrollToShareMembers;
  const [text, setText] = useState("");
  const [fileName, setFileName] = useState<string | null>(null);
  const [grant, setGrant] = useState<ShareInviteRoleId>(DATAPACK_SHARE_GRANT_CONTRIBUTOR);
  const [submitting, setSubmitting] = useState(false);
  const [results, setResults] = useState<DatapackBulkShareResult[] | null>(null);
  /** Set only when the BULK endpoint 404s. Single-invite never sees this
   *  because /share has been on main forever, so we don't let a single
   *  invite trigger the "unavailable" degradation. */
  const [bulkUnavailable, setBulkUnavailable] = useState(false);

  const parse = useMemo(() => parseBulkShareEmails(text), [text]);
  const selfEmail = currentUserEmail?.trim().toLowerCase();
  // Self-invites are pointless (the caller is owner/maintainer already) and
  // the single-invite field also blocks them, so keep them out of the payload.
  const emails = useMemo(
    () => (selfEmail ? parse.emails.filter((e) => e !== selfEmail) : parse.emails),
    [parse.emails, selfEmail],
  );
  const selfSkipped = parse.emails.length - emails.length > 0;
  const overCap = emails.length > DATAPACK_BULK_SHARE_MAX_EMAILS;
  const summary = useMemo(() => (results ? summarizeBulkShareResults(results) : null), [results]);

  const handleTextChange = (next: string) => {
    setText(next);
    // Clear the previous results on any input change so the stale table
    // never lingers next to a new list.
    setResults(null);
  };

  const submitSingle = async (email: string) => {
    await inviteDatapackShare(datapackId, { recipient_email: email, grant }, {});
    trackEvent(TELEMETRY_EVENTS.DATAPACK_SHARED, {
      datapack_id: datapackId,
      shared_by_email: currentUserEmail,
      invited_email: email,
      shared_by_role: sharedByRole ?? undefined,
      invited_role: grant,
      is_first_share: priorShareCount === 0,
      total_shares: priorShareCount + 1,
      shared_timestamp: nowIso(),
    });
    toast({ title: "Invite sent" });
    setText("");
    setFileName(null);
  };

  /** Returns `allClean` — true when every address resulted in a NEW invite.
   *  Caller uses this to decide whether to close an enclosing dialog or keep
   *  it open so the user can read the per-row results panel for mixed /
   *  all-unchanged / failed batches. */
  const submitBulk = async (list: string[]): Promise<boolean> => {
    const res = await bulkShareDatapack(datapackId, { emails: list, grant }, {});
    const rows = res.results ?? [];
    setResults(rows);
    const rolled = summarizeBulkShareResults(rows);
    // One rolled-up telemetry event for the whole batch — but only when at
    // least one row actually resulted in an invite. The single-invite path
    // gates its DATAPACK_SHARED on a successful create; firing here on a
    // batch of all-typos / already-members / errors would report
    // is_first_share:true + total_shares:priorShareCount and pollute the
    // funnel with attempts, not shares. If we ever want an "attempt"
    // signal, that belongs on a separate event name, not this one.
    if (rolled.invited > 0) {
      trackEvent(TELEMETRY_EVENTS.DATAPACK_SHARED, {
        datapack_id: datapackId,
        shared_by_email: currentUserEmail,
        shared_by_role: sharedByRole ?? undefined,
        invited_role: grant,
        invite_count: list.length,
        results_invited: rolled.invited,
        results_unchanged: rolled.unchanged,
        results_failed: rolled.failed,
        is_first_share: priorShareCount === 0,
        total_shares: priorShareCount + rolled.invited,
        shared_timestamp: nowIso(),
      });
    }
    toast({
      title:
        rolled.invited > 0
          ? `Invited ${rolled.invited} of ${rolled.total}`
          : "No new invites needed",
      description:
        (rolled.failed > 0
          ? `${rolled.unchanged} already had access · ${rolled.failed} could not be processed.`
          : `${rolled.unchanged} already had access.`) +
        (rolled.unchanged > 0 ? " To change a member's role, use Members on this page." : ""),
      action:
        rolled.unchanged > 0 ? (
          <ToastAction altText="Go to Members" onClick={goToMembers}>
            Go to Members
          </ToastAction>
        ) : undefined,
      variant: rolled.invited === 0 && rolled.failed > 0 ? "destructive" : undefined,
    });
    return rolled.failed === 0 && rolled.unchanged === 0;
  };

  const handleSubmit = async () => {
    if (emails.length === 0) {
      toast({
        variant: "destructive",
        title: "No addresses to invite",
        description: "Type an email, paste a list, or upload a CSV with an email column.",
      });
      return;
    }
    if (overCap) {
      toast({
        variant: "destructive",
        title: "Too many addresses",
        description: `Up to ${DATAPACK_BULK_SHARE_MAX_EMAILS} addresses per batch — you have ${emails.length}. Split the list and run it twice.`,
      });
      return;
    }

    setSubmitting(true);
    setResults(null);
    try {
      let allClean = true;
      if (emails.length === 1) {
        await submitSingle(emails[0]);
      } else {
        allClean = await submitBulk(emails);
      }
      await onInvitesSent(allClean);
    } catch (err) {
      if (emails.length >= 2 && isApiEndpointMissingError(err)) {
        // /bulk-share not deployed. /share was fine; only the batch
        // path degrades to the amber notice.
        setBulkUnavailable(true);
        return;
      }
      if (emails.length === 1 && isMekoApiRequestError(err) && err.code === "ALREADY_MEMBER") {
        toast({
          title: "Already a member",
          description: `${emails[0]} already has access. To change their role, use Members on this page.`,
          action: (
            <ToastAction altText="Go to Members" onClick={goToMembers}>
              Go to Members
            </ToastAction>
          ),
        });
        return;
      }
      toast({
        variant: "destructive",
        title: emails.length === 1 ? "Invite failed" : "Bulk invite failed",
        description: getApiErrorMessage(
          err,
          emails.length === 1 ? "Could not send invite." : "Could not grant access to this list.",
        ),
      });
    } finally {
      setSubmitting(false);
    }
  };

  if (bulkUnavailable) {
    // "Continue with one address" resets the form to the FIRST valid
    // address in the current input (or blank if there wasn't one) —
    // otherwise the textarea still holds N ≥ 2 addresses, the count
    // stays > 1, the next submit routes to /bulk-share again, and the
    // notice re-appears. Trimming the list to one keeps the caller in
    // the flow they came for without silently discarding the whole
    // paste.
    const continueWithOne = () => {
      const first = emails[0] ?? "";
      setText(first);
      setFileName(null);
      setResults(null);
      setBulkUnavailable(false);
    };
    return (
      <WarningBanner>
        <p className="font-medium">Bulk sharing isn&apos;t available on this server yet</p>
        <p className="mt-1 text-xs font-normal">
          This deployment doesn&apos;t expose the bulk-share endpoint. Invite people one at a time —
          the input still works with a single address until it&apos;s enabled.
        </p>
        <div className="mt-3">
          <Button type="button" variant="outline" size="sm" onClick={continueWithOne}>
            {emails[0] ? `Continue with ${emails[0]}` : "Start over with one address"}
          </Button>
        </div>
      </WarningBanner>
    );
  }

  const submitLabel = submitting
    ? emails.length <= 1
      ? "Sending…"
      : `Inviting ${emails.length}…`
    : emails.length <= 1
      ? "Send invite"
      : `Send ${emails.length} invites`;

  return (
    <div className="space-y-5">
      <p className="text-sm text-muted-foreground">
        Grant a role to one person or a whole team. Type an address, paste a list, or upload a CSV —
        commas, semicolons, new lines, and a header row all work. Re-inviting someone who already
        has access is a no-op.
      </p>

      <BulkInviteEmailInput
        text={text}
        onTextChange={handleTextChange}
        onFileAdded={setFileName}
        disabled={submitting}
        parse={parse}
        emails={emails}
        selfSkipped={selfSkipped}
        overCap={overCap}
        fileName={fileName}
        onClear={() => {
          setText("");
          setFileName(null);
          setResults(null);
        }}
      />

      <InviteRoleTiles
        value={grant}
        onChange={setGrant}
        disabled={submitting}
        label="Role for everyone in this list"
      />

      <div className="flex justify-end">
        <Button
          type="button"
          disabled={submitting || emails.length === 0 || overCap}
          className="w-full shrink-0 bg-primary text-primary-foreground sm:w-auto"
          onClick={() => void handleSubmit()}
        >
          {submitLabel}
        </Button>
      </div>

      {summary && results ? <BulkInviteResultsPanel results={results} summary={summary} /> : null}
    </div>
  );
}
