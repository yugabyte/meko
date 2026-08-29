// Agent deployment API calls (deploy adlc-review-agent to AWS Bedrock
// AgentCore, managed or BYOC). Reuses mekoApi's `request` (envelope unwrap +
// errors), same convention as ampApi. Field names mirror
// api_server/internal/models/deployment.go's JSON tags exactly.

import { request } from "@/lib/api/mekoApi";

export type DeploymentRuntime = "agentcore" | "vercel" | "lyzr";
export type DeploymentAccountMode = "managed" | "byoc";
export type DeploymentStatus = "pending" | "deploying" | "active" | "failed" | "deleting" | "deleted";

export type Deployment = {
  id: string;
  runtime: DeploymentRuntime;
  account_mode: DeploymentAccountMode;
  agent_slug: string;
  status: DeploymentStatus;
  status_message?: string;
  agent_runtime_arn?: string;
  container_uri?: string;
  created_at: string;
  updated_at: string;
};

export type ByocInitResponse = {
  id: string;
  external_id: string;
  trust_policy_json: string;
};

/** POST /deployments/byoc/init — BYOC onboarding step 1: mints a pending
 *  deployment + the trust-policy JSON to paste into the caller's own AWS
 *  account. */
export async function initByocDeployment(agentSlug: string) {
  return request<ByocInitResponse>("/deployments/byoc/init", "POST", { agent_slug: agentSlug });
}

/** POST /deployments/:id/byoc/confirm — BYOC onboarding step 2: submits the
 *  role ARN the caller created and starts provisioning. */
export async function confirmByocDeployment(id: string, roleArn: string) {
  return request<Deployment>(`/deployments/${encodeURIComponent(id)}/byoc/confirm`, "POST", {
    role_arn: roleArn,
  });
}

/** POST /deployments — managed-mode deployment into Meko's own AWS account. */
export async function createManagedDeployment(agentSlug: string) {
  return request<Deployment>("/deployments", "POST", { agent_slug: agentSlug });
}

/** GET /deployments/:id — also the status-poll target. */
export async function getDeployment(id: string, options: { signal?: AbortSignal } = {}) {
  return request<Deployment>(`/deployments/${encodeURIComponent(id)}`, "GET", undefined, options);
}
