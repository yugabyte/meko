package functional

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/kb_document_lifecycle",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    25 * time.Minute,
		Description: "Knowledge-base documents through both upload paths and out again. The web app's presigned upload (upload-url, the browser's POST to object storage, " +
			"upload-complete) and the direct upload both reach COMPLETED; the documents list, counts, summary, activity log and source status agree; knowledgebase_search " +
			"finds each; the download URL returns the bytes uploaded; a viewer of the shared datapack searches but cannot delete, a stranger sees nothing; a document deleted " +
			"over REST and one deleted with knowledgebase_delete_document leave every view; an upload never completed is listed as pending until it is discarded.",
		Run: kbDocumentLifecycle,
	})
}

type kbDoc struct {
	ID       string `json:"id"`
	Name     string `json:"document_name"`
	URI      string `json:"document_uri"`
	Status   string `json:"status"`
	Chunks   *int64 `json:"chunks_processed"`
	Uploader string `json:"uploaded_by_email"`
}

func kbDocumentLifecycle(t *core.T) error {
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
	dp, err := ownedDatapack(t, owner, "kb-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dp.ID, "viewer"); err != nil {
		return err
	}
	s3, closeS3 := objectStore(t)
	defer closeS3()

	contentA := []byte(fmt.Sprintf("Lighthouse log %s: the keeper repaints the lamp room with cobalt paint every spring.\n", mk))
	contentB := []byte(fmt.Sprintf("Orchard ledger %s: the saffron pears ripen in the north row before the first frost.\n", mk))
	nameA, nameB := "lighthouse-"+mk+".txt", "orchard-"+mk+".txt"
	var keyA, keyB string

	if err := t.Step("presigned_upload_completes", func(h *core.StepHandle) error {
		var err error
		keyA, err = presignedUpload(t, owner, s3, dp.ID, nameA, contentA)
		h.Set("key", keyA)
		if err != nil {
			return err
		}
		return waitIndexed(t, owner.api, dp.ID, keyA)
	}); err != nil {
		return err
	}
	if err := t.Step("direct_upload_completes", func(h *core.StepHandle) error {
		up, err := owner.api.UploadKBFile(t, dp.ID, nameB, contentB)
		if err != nil {
			return err
		}
		keyB = up.DocumentKey
		h.Set("key", keyB)
		return waitIndexed(t, owner.api, dp.ID, keyB)
	}); err != nil {
		return err
	}

	docs := func(u *user) (map[string]kbDoc, error) {
		var page struct {
			Items []kbDoc `json:"items"`
			Total int     `json:"total"`
		}
		if err := u.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/documents?page_size=100", nil, httpOK, &page); err != nil {
			return nil, err
		}
		out := map[string]kbDoc{}
		for _, d := range page.Items {
			out[d.Name] = d
		}
		return out, nil
	}
	type counts struct {
		Docs   int `json:"total_docs"`
		Chunks int `json:"total_chunks"`
	}
	countsOf := func() (counts, error) {
		var c counts
		err := owner.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/counts", nil, httpOK, &c)
		return c, err
	}
	var errs []error
	var byName map[string]kbDoc
	if err := t.Step("every_view_lists_both", func(h *core.StepHandle) error {
		var err error
		if byName, err = docs(owner); err != nil {
			return err
		}
		for _, n := range []string{nameA, nameB} {
			d, ok := byName[n]
			if !ok {
				return fmt.Errorf("the documents list lacks %s: %v", n, byName)
			}
			if d.Status != "COMPLETED" || d.Chunks == nil || *d.Chunks < 1 || d.Uploader != owner.acct.Email {
				return fmt.Errorf("document %s is listed as %+v, want COMPLETED with chunks, uploaded by %s", n, d, owner.acct.Email)
			}
		}
		c, err := countsOf()
		if err != nil {
			return err
		}
		var sum struct {
			Files int `json:"total_files"`
		}
		if err := owner.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/summary", nil, httpOK, &sum); err != nil {
			return err
		}
		var activity struct {
			Items []map[string]any `json:"items"`
		}
		if err := owner.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/activity-log?limit=50", nil, httpOK, &activity); err != nil {
			return err
		}
		h.Set("counts", fmt.Sprintf("%+v", c))
		h.Set("activity", len(activity.Items))
		if c.Docs != 2 || c.Chunks < 2 || sum.Files != 2 {
			return fmt.Errorf("counts %+v and summary %d files, want 2 documents with a chunk each", c, sum.Files)
		}
		if !strings.Contains(fmt.Sprint(activity.Items), nameA) || !strings.Contains(fmt.Sprint(activity.Items), nameB) {
			return fmt.Errorf("the activity log does not name both uploads: %s", truncate(fmt.Sprint(activity.Items), 300))
		}
		return owner.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/status", nil, httpOK, nil)
	}); err != nil {
		errs = append(errs, err)
	}

	conv, err := owner.conversation(dp.ID, "kb "+mk)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	kbSearch := func(u *user, q string) (string, *toolError) {
		return u.call("knowledgebase_search", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "query": q})
	}
	if err := t.Step("search_finds_each", func(*core.StepHandle) error {
		for _, q := range []string{"lamp room cobalt paint " + mk, "saffron pears north row " + mk} {
			text, terr := kbSearch(owner, q)
			if terr != nil {
				return terr
			}
			word := strings.Fields(q)[0]
			if !strings.Contains(text, word) || !strings.Contains(text, mk) {
				return fmt.Errorf("knowledgebase_search for %q did not return its document: %s", q, truncate(text, 300))
			}
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("download_returns_the_bytes", func(*core.StepHandle) error {
		d, ok := byName[nameA]
		if !ok {
			return fmt.Errorf("no listed document %s to download", nameA)
		}
		var link struct {
			URL string `json:"url"`
		}
		if err := owner.rest(post, "/datapacks/"+dp.ID+"/knowledge-bases/download-url", map[string]string{"document_uri": d.URI}, httpOK, &link); err != nil {
			return err
		}
		got, err := s3.get(t, link.URL)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, contentA) {
			return fmt.Errorf("the download returned %q, want the uploaded %q", truncate(string(got), 120), truncate(string(contentA), 120))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("viewer_searches_but_cannot_delete", func(*core.StepHandle) error {
		text, terr := kbSearch(viewer, "lamp room cobalt paint "+mk)
		if terr != nil {
			return terr
		}
		if !strings.Contains(text, mk) {
			return fmt.Errorf("the viewer's knowledgebase_search did not return the document: %s", truncate(text, 300))
		}
		d := byName[nameA]
		r, err := viewer.api.Raw(t, del, "/datapacks/"+dp.ID+"/knowledge-bases/documents/"+d.ID, nil)
		if err != nil {
			return err
		}
		if r.Status != http.StatusForbidden && r.Status != http.StatusNotFound {
			return fmt.Errorf("the viewer deleting a document answered %d, want 403 or 404: %s", r.Status, truncateBody(r.Body))
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("stranger_sees_nothing", func(*core.StepHandle) error {
		for _, p := range []string{"/knowledge-bases/documents", "/knowledge-bases/counts", "/knowledge-bases/summary"} {
			if err := refused(t, "a stranger GET "+p, stranger.api, get, "/datapacks/"+dp.ID+p, nil, statusNotFound, "", nameA, mk); err != nil {
				return err
			}
		}
		return toolRefused(stranger, "a stranger's knowledgebase_search", "knowledgebase_search",
			map[string]any{"datapack_id": dp.ID, "conversation_id": "", "query": "lamp room " + mk}, "cobalt", mk)
	}); err != nil {
		errs = append(errs, err)
	}

	gone := func(q, word string) error {
		return eventually(t, "knowledgebase_search to stop returning "+word, visibleWithin(t), func() (bool, error) {
			text, terr := kbSearch(owner, q)
			if terr != nil {
				return false, terr
			}
			return !strings.Contains(text, word), nil
		})
	}
	if err := t.Step("rest_delete_leaves_every_view", func(*core.StepHandle) error {
		d := byName[nameA]
		if err := owner.rest(del, "/datapacks/"+dp.ID+"/knowledge-bases/documents/"+d.ID, nil, httpOK, nil); err != nil {
			return err
		}
		if err := gone("lamp room cobalt paint "+mk, "Lighthouse"); err != nil {
			return err
		}
		left, err := docs(owner)
		if err != nil {
			return err
		}
		if _, ok := left[nameA]; ok {
			return fmt.Errorf("the documents list still has %s after its delete", nameA)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("mcp_delete_leaves_every_view", func(*core.StepHandle) error {
		d, ok := byName[nameB]
		if !ok {
			return fmt.Errorf("no listed document %s", nameB)
		}
		if _, terr := owner.call("knowledgebase_delete_document", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "document_id": d.ID}); terr != nil {
			return terr
		}
		if err := gone("saffron pears north row "+mk, "Orchard"); err != nil {
			return err
		}
		return eventually(t, "the counts to drop to 0 documents", visibleWithin(t), func() (bool, error) {
			c, err := countsOf()
			return c.Docs == 0, err
		})
	}); err != nil {
		errs = append(errs, err)
	}

	if err := t.Step("abandoned_upload_is_pending_until_discarded", func(h *core.StepHandle) error {
		content := []byte("never confirmed " + mk + "\n")
		key, err := presignedPut(t, owner, s3, dp.ID, "abandoned-"+mk+".txt", content)
		h.Set("key", key)
		if err != nil {
			return err
		}
		pending := func() ([]string, error) {
			var p struct {
				Items []struct {
					Key string `json:"key"`
				} `json:"items"`
			}
			err := owner.rest(get, "/datapacks/"+dp.ID+"/knowledge-bases/pending-uploads", nil, httpOK, &p)
			return idsOf(p.Items, func(i struct {
				Key string `json:"key"`
			}) string {
				return i.Key
			}), err
		}
		keys, err := pending()
		if err != nil {
			return err
		}
		if !contains(keys, key) {
			return fmt.Errorf("an upload never confirmed is not pending: %v, want %s", keys, key)
		}
		if err := owner.rest(del, "/datapacks/"+dp.ID+"/knowledge-bases/pending-uploads?key="+url.QueryEscape(key), nil, httpOK, nil); err != nil {
			return err
		}
		keys, err = pending()
		if err != nil {
			return err
		}
		if contains(keys, key) {
			return fmt.Errorf("%s is still pending after its discard", key)
		}
		return nil
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// store reaches the stack's object store the way a browser does, at the
// address a presigned URL names, through a port-forward. The URL's host stays
// in the Host header because the signature covers it.
type store struct{ addr string }

func objectStore(t *core.T) (*store, func()) {
	kube := clusterEnv(t).Kube()
	pf, err := kube.PortForward(context.Background(), "svc/minio", 0, 9000)
	if err != nil {
		t.Fatalf("forward the object store: %v", err)
	}
	return &store{addr: pf.LocalAddr()}, func() { _ = pf.Close() }
}

func (s *store) do(ctx context.Context, req *http.Request) ([]byte, int, error) {
	req.Host = req.URL.Host
	req.URL.Host = s.addr
	req.URL.Scheme = "http"
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return b, resp.StatusCode, err
}

func (s *store) get(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	b, status, err := s.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET the download URL answered %d: %s", status, truncate(string(b), 200))
	}
	return b, nil
}

// presignedPut asks for an upload URL and sends the file to object storage,
// without confirming it.
func presignedPut(t *core.T, u *user, s *store, datapack, filename string, content []byte) (string, error) {
	sum := sha256.Sum256(content)
	var up struct {
		URL    string            `json:"url"`
		Fields map[string]string `json:"fields"`
		Key    string            `json:"key"`
	}
	if err := u.rest(post, "/datapacks/"+datapack+"/knowledge-bases/upload-url",
		map[string]any{"filename": filename, "size": len(content), "content_hash": hex.EncodeToString(sum[:])}, httpOK, &up); err != nil {
		return "", err
	}
	if up.URL == "" || up.Key == "" {
		return "", fmt.Errorf("upload-url answered %+v, want a URL and a key", up)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range up.Fields {
		_ = mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return up.Key, err
	}
	_, _ = part.Write(content)
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, up.URL, &body)
	if err != nil {
		return up.Key, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	b, status, err := s.do(t, req)
	if err != nil {
		return up.Key, fmt.Errorf("POST the file to the presigned URL: %w", err)
	}
	if status/100 != 2 {
		return up.Key, fmt.Errorf("the presigned POST answered %d: %s", status, truncate(string(b), 300))
	}
	return up.Key, nil
}

// presignedUpload is presignedPut followed by upload-complete, the browser's whole path.
func presignedUpload(t *core.T, u *user, s *store, datapack, filename string, content []byte) (string, error) {
	key, err := presignedPut(t, u, s, datapack, filename, content)
	if err != nil {
		return key, err
	}
	var done struct {
		Key string `json:"key"`
	}
	if err := u.rest(post, "/datapacks/"+datapack+"/knowledge-bases/upload-complete",
		map[string]any{"key": key, "filename": filename, "size": len(content)}, httpOK, &done); err != nil {
		return key, err
	}
	if done.Key != key {
		return key, fmt.Errorf("upload-complete confirmed %q, want %q", done.Key, key)
	}
	return key, nil
}
