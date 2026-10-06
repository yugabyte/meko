package functional

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/artifacts_roundtrip",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    15 * time.Minute,
		Description: "Artifacts a small and a large file take two stores, the database under 1 MiB and object storage above, and both come back byte for byte: artifact_put and " +
			"the REST put answer the content hash, the list, the metadata GET, artifact_get and the download URL agree on the bytes; putting the same bytes again keeps one " +
			"artifact; a viewer of the shared datapack reads it, a stranger gets 404 without the content; a file over the 5 MiB limit is refused with a clear error.",
		Run: artifactsRoundtrip,
	})
}

type artifactMeta struct {
	ID          string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size_bytes"`
	StoredIn    string `json:"stored_in"`
	Inline      string `json:"content_base64"`
	Large       string `json:"content_base64_large"`
	LocalPath   string `json:"local_path"`
}

func artifactsRoundtrip(t *core.T) error {
	accts, done := accountsFor(t, 3)
	defer done()
	var us [3]*user
	for i := range us {
		u, err := newUser(t, accts[i])
		defer u.close()
		if err != nil {
			return err
		}
		us[i] = u
	}
	owner, viewer, stranger := us[0], us[1], us[2]
	mk := marker(t)
	dp, err := ownedDatapack(t, owner, "art-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dp.ID, "viewer"); err != nil {
		return err
	}
	conv, err := owner.conversation(dp.ID, "artifacts "+mk)
	if err != nil {
		return err
	}
	s3, closeS3 := objectStore(t)
	defer closeS3()

	small := []byte("small artifact " + mk + "\n")
	large := bytes.Repeat([]byte("large artifact "+mk+" line of filler text.\n"), (2<<20)/40)
	hashOf := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

	put := func(name string, content []byte) (artifactMeta, error) {
		var m artifactMeta
		err := owner.callJSON("artifact_put", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "filename": name,
			"content_type": "text/plain", "content_base64": base64.StdEncoding.EncodeToString(content)}, &m)
		return m, err
	}
	bytesOf := func(m artifactMeta) ([]byte, error) {
		enc := m.Inline
		if enc == "" {
			enc = m.Large
		}
		return base64.StdEncoding.DecodeString(enc)
	}

	var errs []error
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	for _, c := range []struct {
		name    string
		content []byte
		store   string
	}{{"small-" + mk + ".txt", small, "db"}, {"large-" + mk + ".txt", large, "s3"}} {
		step("roundtrip_"+c.store, func() error {
			m, err := put(c.name, c.content)
			if err != nil {
				return err
			}
			if m.ContentHash != hashOf(c.content) || m.StoredIn != c.store || m.Size != int64(len(c.content)) {
				return fmt.Errorf("artifact_put answered %+v, want hash %s stored in %s with %d bytes", m, hashOf(c.content), c.store, len(c.content))
			}
			var list struct {
				Items []artifactMeta `json:"items"`
			}
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/artifacts?page_size=100", nil, httpOK, &list); err != nil {
				return err
			}
			n := 0
			for _, it := range list.Items {
				if it.ContentHash == m.ContentHash {
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("the artifact list holds %d entries for %s, want 1", n, m.ContentHash)
			}
			var got artifactMeta
			if err := owner.rest(get, "/datapacks/"+dp.ID+"/artifacts/"+m.ContentHash, nil, httpOK, &got); err != nil {
				return err
			}
			b, err := bytesOf(got)
			if err != nil || !bytes.Equal(b, c.content) {
				return fmt.Errorf("the REST GET returned %d bytes (%v), want the %d put", len(b), err, len(c.content))
			}
			var viaMCP artifactMeta
			if err := owner.callJSON("artifact_get", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "content_hash": m.ContentHash}, &viaMCP); err != nil {
				return err
			}
			if b, err := bytesOf(viaMCP); err != nil || !bytes.Equal(b, c.content) {
				if viaMCP.LocalPath != "" {
					return fmt.Errorf("artifact_get over remote MCP answered no content for the %d-byte %s artifact, only local_path %q, a path on the MCP server's own disk that the agent cannot read",
						len(c.content), c.store, viaMCP.LocalPath)
				}
				return fmt.Errorf("artifact_get returned %d bytes (%v), want the %d put", len(b), err, len(c.content))
			}
			if c.store == "s3" {
				var link struct {
					URL string `json:"url"`
				}
				if err := owner.rest(post, "/datapacks/"+dp.ID+"/artifacts/"+m.ContentHash+"/download-url", nil, httpOK, &link); err != nil {
					return err
				}
				b, err := s3.get(t, link.URL)
				if err != nil {
					return err
				}
				if !bytes.Equal(b, c.content) {
					return fmt.Errorf("the download URL returned %d bytes, want the %d put", len(b), len(c.content))
				}
			}
			again, err := put(c.name, c.content)
			if err != nil {
				return err
			}
			if again.ID != m.ID {
				return fmt.Errorf("putting the same bytes again made artifact %s, want the existing %s", again.ID, m.ID)
			}
			if err := refused(t, "a stranger GETs the artifact", stranger.api, get, "/datapacks/"+dp.ID+"/artifacts/"+m.ContentHash, nil, statusNotFound, "", mk); err != nil {
				return err
			}
			var seen artifactMeta
			if err := restAs(t, viewer.api, get, "/datapacks/"+dp.ID+"/artifacts/"+m.ContentHash, nil, httpOK, &seen); err != nil {
				return fmt.Errorf("the viewer cannot read the artifact: %w", err)
			}
			return nil
		})
	}
	step("over_the_limit_is_refused", func() error {
		huge := bytes.Repeat([]byte("x"), 5<<20+1024)
		text, terr := owner.call("artifact_put", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "filename": "huge-" + mk + ".bin",
			"content_type": "application/octet-stream", "content_base64": base64.StdEncoding.EncodeToString(huge)})
		if terr == nil {
			return fmt.Errorf("a %d-byte artifact was accepted over the 5 MiB limit: %s", len(huge), truncate(text, 200))
		}
		if strings.Contains(terr.text, "Traceback") || !strings.Contains(strings.ToLower(terr.text), "large") && !strings.Contains(strings.ToLower(terr.text), "size") && !strings.Contains(strings.ToLower(terr.text), "limit") {
			return fmt.Errorf("the refusal does not say the file is too large: %s", truncate(terr.text, 300))
		}
		return refused(t, "a REST put over the limit", owner.api, post, "/datapacks/"+dp.ID+"/artifacts",
			map[string]any{"filename": "huge.bin", "content_type": "application/octet-stream", "content_base64": base64.StdEncoding.EncodeToString(huge)}, 413, "ARTIFACT_TOO_LARGE")
	})
	return errors.Join(errs...)
}
