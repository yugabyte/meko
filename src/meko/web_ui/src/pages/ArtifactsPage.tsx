// The files agents saved to this datapack with artifact_put: filter by name, page, and download.

import { useState } from "react";
import { Download, Package } from "lucide-react";
import { api } from "../lib/api";
import { saveBase64 } from "../lib/download";
import { bytes, timeAgo } from "../lib/format";
import type { Artifact, Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Pager } from "../components/Pager";
import { Table } from "../components/Table";
import { Button, Card, Empty, ErrorNote, Input, Loading } from "../components/ui";

export function ArtifactsPage({ datapack }: { datapack: Datapack }) {
  const id = datapack.datapack_id;
  const [q, setQ] = useState("");
  const [page, setPage] = useState(1);
  const [error, setError] = useState<Error>();
  const list = useLoad(() => api.artifacts(id, q, page), [id, q, page]);

  const download = async (a: Artifact) => {
    try {
      const full = await api.artifact(id, a.content_hash);
      saveBase64(a.filename, a.content_type, full.content_base64 ?? "");
    } catch (e) {
      setError(e as Error);
    }
  };

  return (
    <div className="space-y-4">
      <Input className="max-w-xs" placeholder="Filter by name" value={q} onChange={(e) => (setQ(e.target.value), setPage(1))} />
      {error && <ErrorNote error={error} />}
      {list.loading && !list.data ? (
        <Loading />
      ) : list.error ? (
        <ErrorNote error={list.error} />
      ) : !list.data?.items.length ? (
        <Empty icon={<Package className="size-8" />} title={q ? "No artifacts match" : "No artifacts yet"}>
          Agents save files here with <b>artifact_put</b>, up to 5 MB each.
        </Empty>
      ) : (
        <>
          <Card className="overflow-hidden">
            <Table
              rows={list.data.items}
              rowKey={(a) => a.content_hash}
              columns={[
                { label: "Name", cell: (a) => a.filename, className: "max-w-xs truncate" },
                { label: "Type", cell: (a) => a.content_type, className: "text-muted-foreground" },
                { label: "Size", right: true, cell: (a) => bytes(a.size_bytes) },
                { label: "Saved", cell: (a) => timeAgo(a.created_at), className: "text-muted-foreground" },
                {
                  label: "",
                  right: true,
                  cell: (a) => (
                    <Button size="sm" variant="ghost" title="Download" onClick={() => download(a)}>
                      <Download className="size-3.5" />
                    </Button>
                  ),
                },
              ]}
            />
          </Card>
          <Pager
            label={`${list.data.total} artifacts`}
            canPrev={page > 1}
            canNext={list.data.has_more}
            onPrev={() => setPage(page - 1)}
            onNext={() => setPage(page + 1)}
          />
        </>
      )}
    </div>
  );
}
