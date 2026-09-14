// Reads the region out of an AgentCore runtime ARN, so the agent card can say
// where it runs without the UI carrying its own copy of the deploy config.

/** `arn:aws:bedrock-agentcore:us-west-2:123456789012:runtime/foo` -> `us-west-2`.
 *  Null for anything that isn't a well-formed ARN, so callers omit the line
 *  rather than printing a guess. */
export function regionFromRuntimeArn(arn: string | null | undefined): string | null {
  if (!arn) return null;
  const parts = arn.split(":");
  // arn : partition : service : region : account : resource
  if (parts.length < 4 || parts[0] !== "arn") return null;
  const region = parts[3]?.trim();
  return region ? region : null;
}
