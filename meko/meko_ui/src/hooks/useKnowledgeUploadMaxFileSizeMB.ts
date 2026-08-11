// useKnowledgeUploadMaxFileSizeMB — tier-aware per-file KB upload cap in MB.
// Mirrors api_server's LimitsFor().MaxKBFileBytes (5 MB free, 50 MB standard,
// the 1 GB platform RAG ceiling for pro/enterprise) so hint copy and the
// client-side size filter match what the backend will actually accept.

import { useCurrentUser } from "@/hooks/useCurrentUser";
import { limitsFor } from "@/lib/freeTier";
import { KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB } from "@/lib/knowledgeFileUploadRules";

export function useKnowledgeUploadMaxFileSizeMB(): number {
  const { data: user } = useCurrentUser();
  // While /auth/me is still loading, fall back to the conservative Free
  // cap instead of limitsFor(undefined)'s Infinity — never let a Free
  // user stage a file the backend is going to reject.
  if (!user) return KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB;
  return limitsFor(user.user_type).kbFileBytes / (1024 * 1024);
}
