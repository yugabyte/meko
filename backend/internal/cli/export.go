package cli

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// toolExport writes a datapack's export (GET /api/v1/datapacks/:id/export) to f.out, or to the
// file name the server suggests in the current folder. The datapack is the one named, or the default;
// the route is Meko's own, so a Meko Cloud datapack can't be exported.
func toolExport(ctx context.Context, t *tools, f toolFlags, stdout io.Writer) error {
	dp, err := t.resolve(ctx, f.text)
	if err != nil {
		return err
	}
	if dp.cloud {
		return fmt.Errorf("export works only on this Meko's datapacks, and %s is in Meko Cloud", dp.name)
	}
	id := dp.id
	if id == "" {
		if id, err = t.defaultDatapackID(ctx); err != nil {
			return err
		}
		if id == "" {
			return errors.New("there's no default datapack (it was deleted): name one, mekosh export <name|id> " +
				"(mekosh datapacks lists them)")
		}
	}
	c, err := newClient(t.l)
	if err != nil {
		return err
	}
	// G704: the URL is always 127.0.0.1 on the port serve wrote to the data folder.
	url := c.base + "/api/v1/datapacks/" + id + "/export"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := (&http.Client{}).Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("export failed (%s): %s", resp.Status, strings.TrimSpace(string(b)))
	}
	out := f.out
	if out == "" {
		out = suggestedName(resp.Header.Get("Content-Disposition"))
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if f.force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(out, flags, 0o600) //nolint:gosec // G304: the user's own path
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists: pass --force to replace it, or -o another file", out)
	}
	if err != nil {
		return err
	}
	// The server cuts the connection when an export fails partway, so a short body is an error here,
	// and a zip that doesn't open (its index comes last) or lacks its header is one too.
	n, err := io.Copy(file, resp.Body)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = checkExport(out)
	}
	if err != nil {
		_ = os.Remove(out) //nolint:gosec // G703: the file this command just created
		return fmt.Errorf("export failed, nothing saved: %w", err)
	}
	fmt.Fprintf(stdout, "Exported to %s (%d bytes): memories, collective memories, conversations, knowledge files "+
		"and artifacts with their contents and embeddings, and the call log\n", out, n)
	return nil
}

// checkExport opens a downloaded export: a complete zip that holds its datapack.json.
func checkExport(path string) error {
	z, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("the export isn't a complete zip: %w", err)
	}
	defer func() { _ = z.Close() }()
	for _, f := range z.File {
		if f.Name == "datapack.json" {
			return nil
		}
	}
	return errors.New("the export has no datapack.json")
}

// suggestedName is the filename in a Content-Disposition header, without any folders, or a fallback.
func suggestedName(disposition string) string {
	if _, after, ok := strings.Cut(disposition, `filename="`); ok {
		if name, _, ok := strings.Cut(after, `"`); ok && filepath.Base(name) != "." {
			return filepath.Base(name)
		}
	}
	return "meko-export.zip"
}
