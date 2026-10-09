// BYOC onboarding: a 3-step wizard matching api_server's two-call flow
// exactly (POST /deployments/byoc/init, then POST /deployments/:id/byoc/confirm).
// Step 2 (pasting the trust policy into AWS) is pure UI state — there's
// nothing to call until the caller has an ARN to submit.

import { useState } from "react";
import { AlertCircle, Check, Copy, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toast } from "@/hooks/use-toast";
import type { Deployment } from "@/lib/api/deploymentsApi";

import { useConfirmByocDeployment, useInitByocDeployment } from "../../../hooks/useDeployments";

const ROLE_ARN_PATTERN = /^arn:aws:iam::\d{12}:role\/.+$/;

type Step = "request" | "trust-policy" | "role-arn";

function CopyableBlock({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  const handleCopy = async () => {
    await navigator.clipboard.writeText(value);
    setCopied(true);
    toast({ title: "Copied to clipboard" });
    window.setTimeout(() => setCopied(false), 2000);
  };
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center justify-between">
        <Label>{label}</Label>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={handleCopy}
          className="h-6 gap-1 px-2 text-xs"
        >
          {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
          Copy
        </Button>
      </div>
      <pre className="max-h-48 overflow-auto rounded-md border border-border bg-muted/30 p-2 text-xs">
        {value}
      </pre>
    </div>
  );
}

export function ByocRoleSetup({
  agentSlug,
  onDeployed,
}: {
  agentSlug: string;
  onDeployed: (deployment: Deployment) => void;
}) {
  const [step, setStep] = useState<Step>("request");
  const [roleArnInput, setRoleArnInput] = useState("");
  const initMutation = useInitByocDeployment();
  const confirmMutation = useConfirmByocDeployment();

  const handleGenerate = () => {
    initMutation.mutate(agentSlug, { onSuccess: () => setStep("trust-policy") });
  };

  const roleArnValid = ROLE_ARN_PATTERN.test(roleArnInput.trim());

  const handleConfirm = () => {
    if (!initMutation.data || !roleArnValid) return;
    confirmMutation.mutate(
      { id: initMutation.data.id, roleArn: roleArnInput.trim() },
      { onSuccess: (deployment) => onDeployed(deployment) },
    );
  };

  if (step === "request") {
    return (
      <div className="flex flex-col gap-3 rounded-md border border-border p-4">
        <p className="text-sm text-muted-foreground">
          We&apos;ll generate a trust-policy template and a one-time ID to paste into an IAM role in
          your own AWS account. Meko never sees or stores your AWS credentials.
        </p>
        <Button onClick={handleGenerate} disabled={initMutation.isPending} className="self-start">
          {initMutation.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
          Generate setup instructions
        </Button>
        {initMutation.isError && (
          <p className="flex items-center gap-1.5 text-sm text-destructive">
            <AlertCircle className="h-4 w-4 shrink-0" />
            {initMutation.error instanceof Error
              ? initMutation.error.message
              : "Failed to start BYOC setup."}
          </p>
        )}
      </div>
    );
  }

  if (step === "trust-policy") {
    const data = initMutation.data;
    if (!data) return null;
    return (
      <div className="flex flex-col gap-3 rounded-md border border-border p-4">
        <ol className="list-decimal space-y-1 pl-4 text-sm text-muted-foreground">
          <li>In the AWS console, create a new IAM role.</li>
          <li>Paste the trust policy below as its trust relationship.</li>
          <li>
            Attach permissions for ECR pull, CloudWatch Logs, bedrock:InvokeModel, and
            bedrock-agentcore:InvokeAgentRuntime (the last one lets Meko invoke your deployed agent
            when you run a review).
          </li>
          <li>Copy the resulting role&apos;s ARN for the next step.</li>
        </ol>
        <CopyableBlock label="External ID" value={data.external_id} />
        <CopyableBlock label="Trust policy JSON" value={data.trust_policy_json} />
        <Button onClick={() => setStep("role-arn")} className="self-start">
          I&apos;ve created the role
        </Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3 rounded-md border border-border p-4">
      <div>
        <Label htmlFor="byoc-role-arn">Role ARN</Label>
        <Input
          id="byoc-role-arn"
          placeholder="arn:aws:iam::123456789012:role/meko-agentcore-deploy"
          value={roleArnInput}
          onChange={(e) => setRoleArnInput(e.target.value)}
        />
        {roleArnInput.trim().length > 0 && !roleArnValid && (
          <p className="mt-1 text-xs text-destructive">
            Expected an IAM role ARN, e.g. arn:aws:iam::123456789012:role/name.
          </p>
        )}
      </div>
      <Button
        onClick={handleConfirm}
        disabled={!roleArnValid || confirmMutation.isPending}
        className="self-start"
      >
        {confirmMutation.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
        Deploy
      </Button>
      {confirmMutation.isError && (
        <p className="flex items-center gap-1.5 text-sm text-destructive">
          <AlertCircle className="h-4 w-4 shrink-0" />
          {confirmMutation.error instanceof Error
            ? confirmMutation.error.message
            : "Role not assumable. Check that the trust policy was pasted correctly."}
        </p>
      )}
    </div>
  );
}
