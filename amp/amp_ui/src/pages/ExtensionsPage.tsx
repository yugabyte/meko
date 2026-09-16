// Extensions = the images an admin has registered, and the surface for adding
// to them.
//
// Only the registered half is listed. The built-ins ship with the controller
// and nothing here can change them, so showing them would be a list of rows
// with no actions; they are still offered in the picker when creating a cluster.
// Registering is how anything amp does not build becomes selectable there.
//
// Importing is admin-only server-side. The form is hidden for a non-admin
// rather than left to fail with a 403, but the server is the authority: it
// re-checks, and it also does every bit of the validation below (the client
// checks are only there to fail fast on an obvious typo).

import { useState } from "react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ErrorState, LoadingState, PageContainer, PageHeader } from "@/components/data/Page";
import {
  useAdminUser,
  useDeregisterExtension,
  useExtensions,
  useRegisterExtension,
} from "@/lib/api/queries";
import type { Extension, RegisterExtensionInput } from "@/lib/api/types";

/** Split a comma/whitespace-separated list into trimmed, non-empty entries. */
function splitList(v: string): string[] {
  return v
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** Parse `KEY=relative/path` lines into the env_paths map. */
function parseEnvPaths(v: string): { value: Record<string, string>; error?: string } {
  const out: Record<string, string> = {};
  for (const line of v
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)) {
    const i = line.indexOf("=");
    if (i <= 0) return { value: {}, error: `"${line}" is not NAME=path` };
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return { value: out };
}

export default function ExtensionsPage() {
  // The admin identity, not the current one: registering is admin-only, and
  // useCurrentUser is the pool owner elsewhere.
  const { data: admin, isLoading: adminLoading } = useAdminUser();
  const { data: extensions, isLoading, isError, error } = useExtensions();
  const [importOpen, setImportOpen] = useState(false);
  const deregister = useDeregisterExtension(admin?.id);

  const isAdmin = admin != null;
  // Only the imported ones are listed — the built-ins ship with the controller
  // and there is nothing to do to them here. The full catalog is still fetched:
  // the import dialog needs the built-in names to refuse one client-side rather
  // than letting the server 409.
  const registered = (extensions ?? []).filter((e) => !e.builtin);

  function remove(name: string) {
    deregister.mutate(name, {
      onSuccess: () => toast.success(`Removed ${name}`),
      // A 409 here means a branch still uses it — the message says which, and
      // is far more useful than "failed".
      onError: (e) => toast.error(e instanceof Error ? e.message : `Could not remove ${name}`),
    });
  }

  return (
    <PageContainer>
      <PageHeader
        eyebrow="Catalog"
        title="Extensions"
        description="OCI images an admin has registered, which a cluster can then enable alongside the extensions this controller ships."
        action={
          isAdmin ? (
            <Button onClick={() => setImportOpen(true)}>Import extension</Button>
          ) : undefined
        }
      />

      {/* Login is still a stub: the UI acts as an admin when the deployment has
          one (useCurrentUser prefers it). When it has none, importing is
          genuinely unavailable — say why, rather than leave the operator to
          conclude the page is broken. */}
      {!isAdmin && !adminLoading && (
        <p className="mb-6 rounded-md border border-border bg-muted/40 px-3 py-2 text-sm text-muted-foreground">
          Importing requires an admin user and this deployment has none. Create one to import
          extension images.
        </p>
      )}

      {isLoading && <LoadingState label="Loading catalog…" />}
      {isError && <ErrorState error={error} />}

      {extensions && (
        <CatalogTable
          caption={
            isAdmin
              ? "Removing one is refused while a branch still uses it."
              : "Registered by an admin."
          }
          rows={registered}
          onRemove={isAdmin ? remove : undefined}
          removing={deregister.isPending ? deregister.variables : undefined}
          empty="Nothing imported yet."
        />
      )}

      <ImportDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        userId={admin?.id}
        existing={extensions ?? []}
      />
    </PageContainer>
  );
}

function CatalogTable({
  caption,
  rows,
  onRemove,
  removing,
  empty,
}: {
  caption: string;
  rows: Extension[];
  onRemove?: (name: string) => void;
  removing?: string;
  empty?: string;
}) {
  return (
    <section>
      <p className="mb-3 text-xs text-muted-foreground">{caption}</p>
      {rows.length === 0 ? (
        <p className="rounded-lg border border-dashed border-border px-4 py-6 text-sm text-muted-foreground">
          {empty ?? "None."}
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>Image</TableHead>
              <TableHead>Description</TableHead>
              {onRemove && <TableHead className="w-24" />}
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((e) => (
              <TableRow key={e.name}>
                <TableCell className="font-mono">
                  {e.name}
                  {e.preload && (
                    <Badge variant="secondary" className="ml-2">
                      preload
                    </Badge>
                  )}
                  {/* The waiver is the thing a reviewer of this catalog most
                      wants to see at a glance: it is what lets a module built
                      against stock PostgreSQL load at all. */}
                  {e.abi_extra_exempt && e.abi_extra_exempt.length > 0 && (
                    <Badge variant="outline" className="ml-2">
                      ABI waiver
                    </Badge>
                  )}
                </TableCell>
                <TableCell className="font-mono text-xs">{e.version ?? "—"}</TableCell>
                <TableCell className="max-w-md truncate font-mono text-xs" title={e.image}>
                  {e.image}
                </TableCell>
                <TableCell className="text-sm text-muted-foreground">
                  {e.description ?? "—"}
                </TableCell>
                {onRemove && (
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={removing === e.name}
                      onClick={() => onRemove(e.name)}
                    >
                      {removing === e.name ? "Removing…" : "Remove"}
                    </Button>
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

function ImportDialog({
  open,
  onOpenChange,
  userId,
  existing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  userId: string | undefined;
  existing: Extension[];
}) {
  const register = useRegisterExtension(userId);
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [version, setVersion] = useState("");
  const [description, setDescription] = useState("");
  const [preload, setPreload] = useState(false);
  const [library, setLibrary] = useState("");
  const [sharePath, setSharePath] = useState("");
  const [libPath, setLibPath] = useState("");
  const [libraryPath, setLibraryPath] = useState("");
  const [envPaths, setEnvPaths] = useState("");
  const [abiExempt, setAbiExempt] = useState("");
  const [formError, setFormError] = useState<string | null>(null);

  function reset() {
    setName("");
    setImage("");
    setVersion("");
    setDescription("");
    setPreload(false);
    setLibrary("");
    setSharePath("");
    setLibPath("");
    setLibraryPath("");
    setEnvPaths("");
    setAbiExempt("");
    setFormError(null);
  }

  function submit() {
    setFormError(null);
    if (!name.trim()) return setFormError("Name is required.");
    if (existing.some((e) => e.name === name.trim() && e.builtin)) {
      return setFormError(`"${name.trim()}" is a built-in extension and cannot be replaced.`);
    }
    if (!image.trim()) return setFormError("Image is required.");
    const env = parseEnvPaths(envPaths);
    if (env.error) return setFormError(`Data directories: ${env.error}`);

    const layout = {
      ...(sharePath.trim() ? { share_path: sharePath.trim() } : {}),
      ...(libPath.trim() ? { lib_path: libPath.trim() } : {}),
      ...(splitList(libraryPath).length ? { library_path: splitList(libraryPath) } : {}),
      ...(Object.keys(env.value).length ? { env_paths: env.value } : {}),
    };
    const input: RegisterExtensionInput = {
      image: image.trim(),
      preload,
      ...(version.trim() ? { version: version.trim() } : {}),
      ...(description.trim() ? { description: description.trim() } : {}),
      ...(library.trim() ? { library: library.trim() } : {}),
      ...(Object.keys(layout).length ? { layout } : {}),
      ...(splitList(abiExempt).length ? { abi_extra_exempt: splitList(abiExempt) } : {}),
    };

    register.mutate(
      { name: name.trim(), input },
      {
        onSuccess: (e) => {
          toast.success(`Imported ${e.name}`);
          reset();
          onOpenChange(false);
        },
        // The controller's message names the offending field, so show it
        // verbatim rather than a generic failure.
        onError: (err) => setFormError(err instanceof Error ? err.message : "Import failed."),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !register.isPending && onOpenChange(o)}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Import an extension</DialogTitle>
          <DialogDescription>
            Register an OCI image so clusters can enable it. Nothing is deployed here — an endpoint
            mounts the image when a branch enables the extension.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <Row
            label="Name"
            hint="Also the mount directory and the name a cluster enables it by. Lowercase letters, digits, - and _."
          >
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="postgis" />
          </Row>
          <Row
            label="Image"
            hint="Pinned to a tag or @sha256 digest — :latest is refused, because image volumes are pulled only if absent and a cached node would never see a new push."
          >
            <Input
              value={image}
              onChange={(e) => setImage(e.target.value)}
              placeholder="ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"
            />
          </Row>
          <div className="grid grid-cols-2 gap-3">
            <Row label="Version" hint="Shown in the picker.">
              <Input
                value={version}
                onChange={(e) => setVersion(e.target.value)}
                placeholder="3.6.2"
              />
            </Row>
            <Row label="Library" hint="Shared-library name, if it differs from the extension name.">
              <Input value={library} onChange={(e) => setLibrary(e.target.value)} />
            </Row>
          </div>
          <Row label="Description" hint="One line, shown when picking extensions.">
            <Input
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Spatial types, functions and indexes"
            />
          </Row>

          <div className="flex items-center justify-between rounded-lg border border-border p-3">
            <div>
              <Label>Preload</Label>
              <p className="text-xs text-muted-foreground">
                Needs shared_preload_libraries, so enabling it restarts the compute.
              </p>
            </div>
            <Switch checked={preload} onCheckedChange={setPreload} />
          </div>

          <div className="rounded-lg border border-border p-3">
            <h3 className="text-sm font-semibold">Image layout</h3>
            <p className="mb-3 text-xs text-muted-foreground">
              Where this image keeps its pieces, relative to its mount directory. Leave blank for an
              image built by amp. The CloudNativePG images use{" "}
              <span className="font-mono">share</span> and <span className="font-mono">lib</span>.
            </p>
            <div className="grid grid-cols-2 gap-3">
              <Row
                label="Share path"
                hint="Holds the control and SQL files. Default share/postgresql."
              >
                <Input
                  value={sharePath}
                  onChange={(e) => setSharePath(e.target.value)}
                  placeholder="share"
                />
              </Row>
              <Row label="Lib path" hint="Holds the .so files. Default lib/postgresql.">
                <Input
                  value={libPath}
                  onChange={(e) => setLibPath(e.target.value)}
                  placeholder="lib"
                />
              </Row>
            </div>
            <Row
              label="Bundled library directories"
              hint="Directories of system libraries the image ships because the endpoint does not carry them (PostGIS bundles geos/proj/gdal in `system`). Comma-separated."
            >
              <Input
                value={libraryPath}
                onChange={(e) => setLibraryPath(e.target.value)}
                placeholder="system"
              />
            </Row>
            <Row
              label="Data directories"
              hint="One NAME=path per line. Read at runtime by the extension's libraries — without PROJ_DATA, PostGIS installs but coordinate transforms fail."
            >
              <Textarea
                rows={2}
                value={envPaths}
                onChange={(e) => setEnvPaths(e.target.value)}
                placeholder={"PROJ_DATA=share/proj\nGDAL_DATA=share/gdal"}
              />
            </Row>
          </div>

          <Row
            label="ABI waiver modules"
            hint="Modules allowed to differ from the server's ABI string — required for an image built against stock PostgreSQL. Bare names, no .so. Every other compatibility field is still enforced. Leave blank unless you have checked the image."
          >
            <Input
              value={abiExempt}
              onChange={(e) => setAbiExempt(e.target.value)}
              placeholder="postgis-3, postgis_raster-3"
            />
          </Row>

          {formError && (
            <p className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
              {formError}
            </p>
          )}
        </div>

        <DialogFooter className="gap-2">
          <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={register.isPending}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={register.isPending}>
            {register.isPending ? "Importing…" : "Import"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Row({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <Label>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}
