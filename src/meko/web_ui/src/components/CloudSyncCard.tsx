// Settings › Cloud Sync: the switch that shows "Sync to Meko Cloud" on this computer's datapacks. Off
// until the owner turns it on; a sync then asks for a Meko Cloud API key when there's no link yet.

import { useState } from "react";
import { api } from "../lib/api";
import { useCloudLinked } from "../lib/cloud";
import { MEKO_CLOUD, href, navigate } from "../lib/route";
import { useLoad } from "../lib/useLoad";
import { Card, ErrorNote, Spinner, Toggle } from "./ui";

export function CloudSyncCard() {
  const setting = useLoad(() => api.cloudSync(), []);
  const linked = useCloudLinked().data;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const s = setting.data;
  const flip = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      await api.setCloudSync(on);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
      setting.reload();
    }
  };
  return (
    <Card className="space-y-2 p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-0.5">
          <h3 className="font-medium">Cloud Sync</h3>
          <p className="text-sm text-muted-foreground">
            Move a datapack from this computer to Meko Cloud, one at a time: its Sync to Meko Cloud button checks it
            fits your plan, moves everything, then deletes it here.
          </p>
          {s?.enabled && linked === false && (
            <p className="text-xs text-muted-foreground">
              A sync asks for your Meko Cloud API key first, or add it now in{" "}
              <a
                href={href(MEKO_CLOUD)}
                onClick={(e) => (e.preventDefault(), navigate(MEKO_CLOUD))}
                className="underline underline-offset-2"
              >
                Meko Cloud
              </a>
              .
            </p>
          )}
        </div>
        {s ? (
          <Toggle label="Enable Cloud Sync" checked={s.enabled} disabled={busy} onChange={flip} />
        ) : (
          !setting.error && <Spinner />
        )}
      </div>
      {(error ?? setting.error) && <ErrorNote error={(error ?? setting.error)!} />}
    </Card>
  );
}
