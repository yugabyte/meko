// ArtifactRow — one row inside the Artifacts detail table. Filename ·
// size · uploaded · storage tier · Download. The Download button
// hits the presigned-URL endpoint for s3 artifacts (browser opens
// the URL in a new tab) and falls back to the base64 GET path for
// db-inline artifacts (client materialises a Blob and triggers a
// synthetic `<a download>` click).

import { useState } from "react";
import { Download, FileText, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { toast } from "@/hooks/use-toast";
import { formatFileSize } from "@/lib/fileSize";
import { formatRelativeTime } from "@/lib/relativeTime";
import {
  getApiErrorMessage,
  getArtifactBase64,
  requestArtifactDownloadURL,
  type Artifact,
} from "@/lib/api/mekoApi";

type Props = {
  artifact: Artifact;
  datapackId: string;
  nowMs: number;
};

export function ArtifactRow({ artifact, datapackId, nowMs }: Props) {
  const [busy, setBusy] = useState(false);

  const handleDownload = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const resp = await requestArtifactDownloadURL(datapackId, artifact.content_hash);
      if (resp.url) {
        // Presigned S3 URL — open in a new tab so navigation stays intact.
        window.open(resp.url, "_blank", "noopener,noreferrer");
        return;
      }
      // Inline (db): fetch base64 and trigger a client-side download.
      const inline = await getArtifactBase64(datapackId, artifact.content_hash);
      const b64 = inline.content_base64 ?? inline.content_base64_large ?? "";
      if (!b64) throw new Error("Artifact has no downloadable content");
      const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
      const blob = new Blob([bytes as BlobPart], {
        type: artifact.content_type || "application/octet-stream",
      });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = artifact.filename;
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
    } catch (err) {
      toast({
        title: "Download failed",
        description: getApiErrorMessage(err, "Could not download this artifact."),
      });
    } finally {
      setBusy(false);
    }
  };

  // Column widths mirror the header row in DatapackArtifactsPage so
  // filename / size / uploaded / download line up under their labels.
  // Values are left-aligned within their fixed-width column boxes so
  // each value hugs the header label above it — same convention KB uses.
  return (
    <li className="flex flex-wrap items-center gap-4 px-4 py-3">
      <div className="flex min-w-0 flex-1 items-center gap-3">
        <FileText className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="truncate text-sm text-foreground" title={artifact.filename}>
          {artifact.filename}
        </span>
      </div>
      <span className="w-20 shrink-0 text-xs tabular-nums text-muted-foreground">
        {formatFileSize(artifact.size_bytes)}
      </span>
      <span className="w-28 shrink-0 text-xs text-muted-foreground">
        {formatRelativeTime(artifact.created_at, nowMs)}
      </span>
      {/* Brand-orange Download button — pinned to a fixed w-[7.5rem]
          + justify-center so the button width stays constant across
          the "Download" / "Preparing…" swap and the header spacer
          aligns pixel-for-pixel. */}
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={busy}
        onClick={handleDownload}
        className="w-[7.5rem] shrink-0 justify-center gap-1.5 border-brand/40 text-brand hover:bg-brand/10 hover:text-brand"
      >
        {busy ? (
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
        ) : (
          <Download className="h-3.5 w-3.5" aria-hidden />
        )}
        {busy ? "Preparing…" : "Download"}
      </Button>
    </li>
  );
}
