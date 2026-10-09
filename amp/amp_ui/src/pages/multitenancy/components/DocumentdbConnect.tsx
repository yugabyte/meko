// The documentdb gateway's connection string for one compute instance, shaped by
// how a client can verify the certificate the gateway serves.

import { useState } from "react";
import { Check, Copy, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { useEndpointCaBundle } from "@/hooks/useEndpoints";
import type { Endpoint } from "@/lib/api/types";
import { caFileName, documentdbURI, downloadCaFile, documentdbTrust } from "@/lib/endpointTls";
import { dataBranchingMessages as msg } from "@/lib/messages";

export function DocumentdbConnect({
  ep,
  poolId,
  address,
}: {
  ep: Endpoint;
  poolId: string;
  address: string;
}) {
  const [copied, setCopied] = useState(false);
  const bundle = useEndpointCaBundle(poolId, ep, true);
  // A failed lookup (an unreachable Kubernetes API, say) gets the string that
  // connects whatever the gateway serves, but its own message: the cause is
  // unknown, not "self-signed". While loading there is no string rather than a
  // guess.
  const trust = bundle.data
    ? documentdbTrust(bundle.data)
    : bundle.isError
      ? "unverified"
      : undefined;
  const caCrt = bundle.data?.ca_crt;
  const uri = trust ? documentdbURI(address, ep.id, trust) : "";

  function copy() {
    if (!uri || !navigator.clipboard) return;
    navigator.clipboard
      .writeText(uri)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      })
      .catch((err) => {
        toast.error(msg.clipboardError);
        console.error("Clipboard copy failed:", err);
      });
  }

  return (
    <div className="mt-2">
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs text-muted-foreground">
          {uri || msg.documentdbCheckingTrust}
        </code>
        <Button
          variant="outline"
          size="sm"
          onClick={copy}
          disabled={!uri}
          className="h-7 shrink-0 gap-1"
        >
          {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
          {copied ? "Copied" : "Copy"}
        </Button>
        {trust === "ca_file" && caCrt && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => downloadCaFile(ep.id, caCrt)}
            title={`Download the CA as ${caFileName(ep.id)} (tlsCAFile)`}
            className="h-7 shrink-0 gap-1"
          >
            <ShieldCheck className="h-3.5 w-3.5" />
            CA
          </Button>
        )}
      </div>
      {trust === "ca_file" && (
        <p className="mt-1.5 text-[11px] text-muted-foreground">
          {msg.documentdbCaHint(caFileName(ep.id))}
        </p>
      )}
      {trust === "unverified" && (
        <p className="mt-1.5 text-[11px] text-amber-600 dark:text-amber-500">
          {bundle.data ? msg.documentdbUnverified : msg.documentdbTrustUnknown}
        </p>
      )}
    </div>
  );
}
