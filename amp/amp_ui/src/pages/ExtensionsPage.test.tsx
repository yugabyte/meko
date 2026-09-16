import { describe, it, expect, afterEach, vi, beforeEach } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor as waitForRTL } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Extension } from "@/lib/api/types";

const waitFor = (cb: () => void) => waitForRTL(cb, { timeout: 5000 });

// The page's job is to keep two things straight that look alike on screen: what
// this controller ships (not removable, not replaceable) and what an admin
// registered (both). The admin bit gates the writes, so the user fake is the
// other seam worth controlling.
const builtin: Extension = {
  name: "pgvector",
  image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2",
  version: "0.8.2",
  description: "Vector similarity search",
  preload: false,
  builtin: true,
};
const registered: Extension = {
  name: "postgis",
  image: "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm",
  version: "3.6.2",
  description: "Spatial types, functions and indexes",
  preload: false,
  builtin: false,
  abi_extra_exempt: ["postgis-3"],
};

let isAdmin = true;
const registerExtension = vi.fn();
const deregisterExtension = vi.fn();

vi.mock("@/lib/api/resources", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api/resources")>();
  return {
    ...actual,
    // A non-admin first, mirroring a real deployment: the bootstrap user has no
    // admin bit and any admin is appended after it. useCurrentUser must still
    // find the admin, or this page's whole write surface is unreachable.
    listUsers: async () => ({
      users: isAdmin
        ? [
            { id: "user-0", is_admin: false },
            { id: "user-1", is_admin: true },
          ]
        : [{ id: "user-0", is_admin: false }],
      total: isAdmin ? 2 : 1,
      count: isAdmin ? 2 : 1,
    }),
    listExtensions: async () => ({
      extensions: [builtin, registered],
      total: 2,
      count: 2,
    }),
    registerExtension: (...a: unknown[]) => registerExtension(...a),
    deregisterExtension: (...a: unknown[]) => deregisterExtension(...a),
  };
});

const { default: ExtensionsPage } = await import("./ExtensionsPage");

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <ExtensionsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(cleanup);
beforeEach(() => {
  isAdmin = true;
  registerExtension.mockReset();
  deregisterExtension.mockReset();
  registerExtension.mockResolvedValue(registered);
  deregisterExtension.mockResolvedValue(undefined);
});

describe("ExtensionsPage", () => {
  it("lists only the imported images, not what ships with the controller", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());

    // Nothing here can change a built-in, so listing it would be a row with no
    // actions. It stays in the create-cluster picker, not on this page.
    expect(screen.queryByText("pgvector")).toBeNull();
    // The registered one carries the reference it was registered with.
    expect(
      screen.getByText("ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"),
    ).toBeTruthy();
    // ...and is flagged as loading past the server's ABI check, which is the
    // fact a reviewer of this catalog most needs to see.
    expect(screen.getByText(/ABI waiver/i)).toBeTruthy();
  });

  it("offers Remove only for an imported extension, never a built-in", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    // One Remove button: postgis. pgvector is compiled in.
    expect(screen.getAllByRole("button", { name: /remove/i })).toHaveLength(1);
  });

  it("hides the admin surface, and says why, when the deployment has no admin", async () => {
    isAdmin = false;
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    expect(screen.queryByRole("button", { name: /import extension/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /remove/i })).toBeNull();
    // Silently hiding it reads as a broken page; name the reason.
    await waitFor(() =>
      expect(screen.getByText(/requires an admin user and this deployment has none/i)).toBeTruthy(),
    );
  });

  // Regression: the deployed portal had an admin that was not users[0], so the
  // page hid its own import button and the feature was unreachable.
  it("acts as the admin even when it is not the first user", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    expect(screen.getByRole("button", { name: /import extension/i })).toBeTruthy();
  });

  it("sends the layout and waiver an image needs, not just its reference", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /import extension/i }));

    fireEvent.change(screen.getByPlaceholderText("postgis"), {
      target: { value: "postgis" },
    });
    fireEvent.change(
      screen.getByPlaceholderText("ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"),
      { target: { value: "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm" } },
    );
    fireEvent.change(screen.getByPlaceholderText("share"), { target: { value: "share" } });
    fireEvent.change(screen.getByPlaceholderText("lib"), { target: { value: "lib" } });
    fireEvent.change(screen.getByPlaceholderText("system"), { target: { value: "system" } });
    fireEvent.change(screen.getByPlaceholderText(/PROJ_DATA=share\/proj/), {
      target: { value: "PROJ_DATA=share/proj\nGDAL_DATA=share/gdal" },
    });
    fireEvent.change(screen.getByPlaceholderText("postgis-3, postgis_raster-3"), {
      target: { value: "postgis-3, postgis_raster-3" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));

    await waitFor(() => expect(registerExtension).toHaveBeenCalled());
    const [name, userId, input] = registerExtension.mock.calls[0];
    expect(name).toBe("postgis");
    expect(userId).toBe("user-1");
    expect(input.image).toBe("ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm");
    expect(input.layout).toEqual({
      share_path: "share",
      lib_path: "lib",
      library_path: ["system"],
      env_paths: { PROJ_DATA: "share/proj", GDAL_DATA: "share/gdal" },
    });
    expect(input.abi_extra_exempt).toEqual(["postgis-3", "postgis_raster-3"]);
  });

  // Built-ins are hidden but still fetched: without them the dialog could not
  // refuse a built-in's name client-side and would rely on the server's 409.
  it("refuses a name that is already a built-in, without calling the API", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /import extension/i }));

    fireEvent.change(screen.getByPlaceholderText("postgis"), {
      target: { value: "pgvector" },
    });
    fireEvent.change(
      screen.getByPlaceholderText("ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"),
      { target: { value: "ghcr.io/x/y:1" } },
    );
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));

    await waitFor(() => expect(screen.getByText(/built-in extension/i)).toBeTruthy());
    expect(registerExtension).not.toHaveBeenCalled();
  });

  it("shows the controller's own rejection rather than a generic failure", async () => {
    // The server names the offending field; that message is the useful one.
    registerExtension.mockRejectedValue(
      new Error('image "ghcr.io/x/y:latest": :latest is refused'),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /import extension/i }));

    fireEvent.change(screen.getByPlaceholderText("postgis"), { target: { value: "gis" } });
    fireEvent.change(
      screen.getByPlaceholderText("ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"),
      { target: { value: "ghcr.io/x/y:latest" } },
    );
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));

    await waitFor(() => expect(screen.getByText(/:latest is refused/)).toBeTruthy());
  });

  it("rejects a malformed data-directory line before sending it", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /import extension/i }));

    fireEvent.change(screen.getByPlaceholderText("postgis"), { target: { value: "gis" } });
    fireEvent.change(
      screen.getByPlaceholderText("ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm"),
      { target: { value: "ghcr.io/x/y:1" } },
    );
    fireEvent.change(screen.getByPlaceholderText(/PROJ_DATA=share\/proj/), {
      target: { value: "PROJ_DATA share/proj" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));

    await waitFor(() => expect(screen.getByText(/is not NAME=path/)).toBeTruthy());
    expect(registerExtension).not.toHaveBeenCalled();
  });

  it("removes an imported extension by name", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("postgis")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /remove/i }));
    await waitFor(() => expect(deregisterExtension).toHaveBeenCalledWith("postgis", "user-1"));
  });
});
