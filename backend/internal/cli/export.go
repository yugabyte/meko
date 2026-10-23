package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// toolExport writes a datapack's export (GET /api/v1/datapacks/:id/export) to f.out, or to the
// file name the server suggests in the current folder. The datapack is the one named, or the default.
func toolExport(ctx context.Context, t *toolSession, l paths.Layout, f toolFlags, stdout io.Writer) error {
	id, err := t.datapackID(ctx, f.text)
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = t.defaultDatapackID(ctx); err != nil {
			return err
		}
		if id == "" {
			return errors.New("there's no default datapack (it was deleted): name one, meko export <name|id> " +
				"(meko datapacks lists them)")
		}
	}
	c, err := newClient(l)
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
	// The server cuts the connection when an export fails partway, so a short body is an error here;
	// a body that doesn't end the JSON object is one too.
	end := &lastByte{}
	n, err := io.Copy(io.MultiWriter(file, end), resp.Body)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil && end.b != '}' {
		err = errors.New("the export ended early")
	}
	if err != nil {
		_ = os.Remove(out) //nolint:gosec // G703: the file this command just created
		return fmt.Errorf("export failed, nothing saved: %w", err)
	}
	fmt.Fprintf(stdout, "Exported to %s (%d bytes): memories, collective memories, conversations, and the knowledge "+
		"file and artifact lists. Embeddings and file contents aren't included\n", out, n)
	return nil
}

// lastByte remembers the last non-space byte written through it.
type lastByte struct{ b byte }

func (l *lastByte) Write(p []byte) (int, error) {
	for i := len(p) - 1; i >= 0; i-- {
		if c := p[i]; c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			l.b = c
			break
		}
	}
	return len(p), nil
}

// suggestedName is the filename in a Content-Disposition header, without any folders, or a fallback.
func suggestedName(disposition string) string {
	if _, after, ok := strings.Cut(disposition, `filename="`); ok {
		if name, _, ok := strings.Cut(after, `"`); ok && filepath.Base(name) != "." {
			return filepath.Base(name)
		}
	}
	return "meko-export.json"
}
