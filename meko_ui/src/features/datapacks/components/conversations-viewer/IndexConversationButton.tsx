// "Index conversation" action — backfills a past WorkbenchLM conversation into
// the conversation cache. Shown only when the detail reports `can_index`.
// Fetches the per-datapack MCP token, then calls the gateway which embeds each
// turn via MCP.

import { useState } from "react";
import { Database, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { toast } from "@/hooks/use-toast";
import { getDatapackProvisionStatus } from "@/lib/api/mekoApi";
import { indexConversation, getInferenceGatewayErrorMessage } from "@/lib/inferenceGateway";

type Props = {
  datapackId: string;
  conversationId: string;
  /** Called after a successful index so the parent refetches (button hides). */
  onIndexed?: () => void;
};

export function IndexConversationButton({ datapackId, conversationId, onIndexed }: Props) {
  const [busy, setBusy] = useState(false);

  const handleClick = async () => {
    setBusy(true);
    try {
      const ps = await getDatapackProvisionStatus(datapackId);
      const token = ps.mcp_api_key;
      if (!token) throw new Error("MCP token unavailable for this datapack.");
      const res = await indexConversation({
        conversation_id: conversationId,
        datapack_id: datapackId,
        user_token: token,
      });
      toast({
        title: "Conversation indexed successfully",
        description: res.indexed > 0 ? `${res.indexed} turn${res.indexed === 1 ? "" : "s"} added to the cache.` : undefined,
      });
      onIndexed?.();
    } catch (err) {
      toast({ title: "Index failed", description: getInferenceGatewayErrorMessage(err, "Could not index this conversation.") });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Button
      variant="outline"
      size="sm"
      onClick={handleClick}
      disabled={busy}
      className="gap-1.5 border-border bg-background text-foreground/80 hover:text-foreground"
      title="Embed this conversation's turns into the shared answer cache"
    >
      {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden /> : <Database className="h-3.5 w-3.5" aria-hidden />}
      {busy ? "Indexing…" : "Index conversation"}
    </Button>
  );
}
