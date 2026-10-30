// Overview's warning while the memory extractor is off: Meko isn't pulling memories out of this
// datapack's conversations, and where to turn it on: Settings, or the command. Closing it
// lasts until the datapack has more conversations; it's hidden for a datapack that opted out.

import { useState } from "react";
import { BrainCircuit } from "lucide-react";
import { api } from "../lib/api";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Banner } from "./Banner";
import { SettingsHere } from "./SettingsHere";

export const EXTRACTOR_COMMAND = "meko memory-extractor use claude";

const key = (id: string) => `meko.extraction-banner.${id}`;

function dismissedAt(id: string): number {
  try {
    return Number(localStorage.getItem(key(id)) ?? -1);
  } catch {
    return -1;
  }
}

export function ExtractionBanner({ datapack }: { datapack: Datapack }) {
  const id = datapack.datapack_id;
  const conversations = datapack.conversation_count ?? 0;
  const status = useLoad(() => api.memoryExtractor(), []);
  const own = useLoad(() => api.datapackExtraction(id), [id]);
  const [closed, setClosed] = useState(() => dismissedAt(id) >= conversations);
  if (closed || !status.data || status.data.enabled || !own.data || own.data.opt_out) return null;
  const close = () => {
    try {
      localStorage.setItem(key(id), String(conversations));
    } catch {
      // Not remembered: it shows again on the next visit.
    }
    setClosed(true);
  };
  return (
    <Banner tone="warning" icon={<BrainCircuit />} command={EXTRACTOR_COMMAND} onClose={close}>
      <b>Meko isn't extracting memories from your conversations.</b> Point it at the AI tool you already use, and it
      pulls memories out of them in the background, from your own messages. Turn it on in <SettingsHere />, or run
      this in a terminal (Claude Code today; Codex and Cursor are coming):
    </Banner>
  );
}
