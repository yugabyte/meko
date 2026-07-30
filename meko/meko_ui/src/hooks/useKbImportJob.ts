import { useEffect, useState } from "react";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import { KB_IMPORT_JOB_EVENT, type KbImportJobDetail } from "@/lib/knowledgeBaseImportJob";

export function useKbImportJobDetail() {
  const [detail, setDetail] = useState<KbImportJobDetail>({ phase: "idle" });

  useEffect(() => {
    const handler = (ev: Event) => {
      const ce = ev as CustomEvent<KbImportJobDetail>;
      setDetail(ce.detail ?? { phase: "idle" });
    };
    addAppEventListener(KB_IMPORT_JOB_EVENT, handler);
    return () => removeAppEventListener(KB_IMPORT_JOB_EVENT, handler);
  }, []);

  return detail;
}
