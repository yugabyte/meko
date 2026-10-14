// Create-cluster wizard step for the cluster's cloud_admin password: generate a
// strong one (default) or type your own, shown once because the control plane
// keeps only the SCRAM verifier and never returns it.
// Shared: the create-cluster wizard and any add-read_write-compute flow (both
// hit the controller's read_write password requirement).
import { useState } from "react";
import { Check, Copy, Download, Eye, EyeOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { WarningBanner } from "@/components/ui/warning-banner";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";
import { downloadTextFile } from "@/lib/downloadFile";
import { toast } from "@/hooks/use-toast";
import { AMP_PASSWORD_MAX, AMP_PASSWORD_MIN, CLOUD_ADMIN_USER } from "@/lib/ampPassword";
import type { AmpCredentials } from "@/features/datapacks/hooks/useAmpCredentials";

export function AmpCredentialsStep({
  creds,
  clusterName,
}: {
  creds: AmpCredentials;
  clusterName?: string;
}) {
  const [reveal, setReveal] = useState(false);
  const [copied, setCopied] = useState(false);

  const filename = `cloud_admin-${(clusterName || "").trim() || "amp-cluster"}.txt`;
  const fileBody = `username=${CLOUD_ADMIN_USER}\npassword=${creds.generated}\n`;

  const copy = async () => {
    if (await copyToClipboard(creds.generated)) {
      setCopied(true);
      creds.setAcknowledged(true);
      toast({ title: "Password copied" });
    }
  };
  const download = () => {
    downloadTextFile(filename, fileBody);
    creds.setAcknowledged(true);
  };

  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <Label htmlFor="wiz-db-user">Database user</Label>
        <Input id="wiz-db-user" value={CLOUD_ADMIN_USER} disabled className="font-mono" />
        <p className="text-xs text-muted-foreground">
          AMP creates this superuser; you set its password, and it's what clients connect as.
        </p>
      </div>

      <div className="grid grid-cols-2 gap-2">
        <ModeButton
          active={creds.mode === "generate"}
          onClick={() => creds.setMode("generate")}
          title="Generate a strong password"
          sub="Recommended"
        />
        <ModeButton
          active={creds.mode === "custom"}
          onClick={() => creds.setMode("custom")}
          title="Use my own"
          sub="Type a password"
        />
      </div>

      {creds.mode === "generate" ? (
        <div className="space-y-2">
          <Label>Generated password</Label>
          <div className="flex gap-2">
            <Input
              readOnly
              type={reveal ? "text" : "password"}
              value={creds.generated}
              className="font-mono"
            />
            <Button
              type="button"
              variant="outline"
              size="icon"
              aria-label={reveal ? "Hide password" : "Reveal password"}
              onClick={() => setReveal((r) => !r)}
            >
              {reveal ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
            </Button>
            <Button
              type="button"
              variant="outline"
              size="icon"
              aria-label="Copy password"
              onClick={copy}
            >
              {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
            </Button>
          </div>
          <Button type="button" variant="outline" size="sm" onClick={download}>
            <Download className="mr-1.5 h-4 w-4" /> Download credentials
          </Button>
        </div>
      ) : (
        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="wiz-db-pass">Password</Label>
            <Input
              id="wiz-db-pass"
              type="password"
              placeholder={`${AMP_PASSWORD_MIN}–${AMP_PASSWORD_MAX} printable ASCII characters`}
              value={creds.custom}
              onChange={(e) => creds.setCustom(e.target.value)}
              aria-invalid={Boolean(creds.customError)}
              className="font-mono"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="wiz-db-confirm">Confirm password</Label>
            <Input
              id="wiz-db-confirm"
              type="password"
              value={creds.confirm}
              onChange={(e) => creds.setConfirm(e.target.value)}
              aria-invalid={creds.confirmMismatch}
              className="font-mono"
            />
          </div>
          {creds.customError ? (
            <p className="text-xs text-red-700 dark:text-red-300" role="alert">
              {creds.customError}
            </p>
          ) : creds.confirmMismatch ? (
            <p className="text-xs text-red-700 dark:text-red-300" role="alert">
              Passwords do not match.
            </p>
          ) : null}
        </div>
      )}

      <WarningBanner>
        This sets the <code>{CLOUD_ADMIN_USER}</code> password and is <strong>shown once</strong> —
        AMP stores only a verifier and can never show it again.{" "}
        {creds.mode === "generate"
          ? "Copy or download it before you continue."
          : "Save it somewhere safe before you continue."}
      </WarningBanner>
    </div>
  );
}

// Local segmented toggle, matching the wizard's engine/extension pickers. Not
// exported: nothing outside this step needs it.
function ModeButton({
  active,
  onClick,
  title,
  sub,
}: {
  active: boolean;
  onClick: () => void;
  title: string;
  sub: string;
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "rounded-lg border px-3 py-2.5 text-left transition-colors",
        active
          ? "border-brand bg-brand/10 ring-1 ring-brand"
          : "border-border bg-background hover:bg-muted/50",
      )}
    >
      <span className="block text-sm font-medium text-foreground">{title}</span>
      <span className="mt-0.5 block text-xs text-muted-foreground">{sub}</span>
    </button>
  );
}
