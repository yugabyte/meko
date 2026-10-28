package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/knowledge"
)

// uploadTimeout bounds mekosh upload: a 50 MiB file takes many minutes to index.
const uploadTimeout = time.Hour

// uploadPoll is how often mekosh upload asks how a file is doing.
const uploadPoll = time.Second

// uploadOffer is where to upload a file to, or that the datapack already has its bytes.
type uploadOffer struct {
	URL              string            `json:"url"`
	Fields           map[string]string `json:"fields"`
	Key              string            `json:"key"`
	AlreadyUploaded  bool              `json:"already_uploaded"`
	ExistingFilename string            `json:"existing_filename"`
}

// knowledgeBase is a datapack's knowledge base as mekosh upload sees it: this Meko's, through its
// tools, or the linked Meko Cloud's, through its REST API. Each call returns the reply's raw JSON
// too, for --json.
type knowledgeBase interface {
	offer(ctx context.Context, name string, data []byte) (uploadOffer, string, error)
	status(ctx context.Context, key string) (fileState, string, error)
}

// localKB is this Meko's: knowledgebase_upload_document and knowledgebase_document_status.
type localKB struct {
	t  *toolSession
	dp string
}

func (k localKB) offer(ctx context.Context, name string, data []byte) (uploadOffer, string, error) {
	sum := sha256.Sum256(data)
	reply, err := k.t.call(ctx, "knowledgebase_upload_document", map[string]any{"filename": name, "size": len(data),
		"content_hash": hex.EncodeToString(sum[:]), "datapack_id": k.dp,
		"content_head_base64": base64.StdEncoding.EncodeToString(data[:min(len(data), knowledge.SniffBytes)])})
	if err != nil {
		return uploadOffer{}, reply, err
	}
	var o uploadOffer
	return o, reply, json.Unmarshal([]byte(reply), &o)
}

func (k localKB) status(ctx context.Context, key string) (fileState, string, error) {
	reply, err := k.t.call(ctx, "knowledgebase_document_status", map[string]any{"document_key": key, "datapack_id": k.dp})
	if err != nil {
		return fileState{}, reply, err
	}
	var st fileState
	return st, reply, json.Unmarshal([]byte(reply), &st)
}

// cloudKB is the linked Meko Cloud's, through the running server's upload-url and document-status
// routes, which send them to Meko Cloud's (its REST API takes no content_head_base64).
type cloudKB struct {
	t  *tools
	dp string
}

func (k cloudKB) offer(ctx context.Context, name string, data []byte) (uploadOffer, string, error) {
	sum := sha256.Sum256(data)
	reply, err := k.t.cloudCall(ctx, http.MethodPost, cloudDatapackPath(k.dp, "/knowledge-bases/upload-url"),
		map[string]any{"filename": name, "size": len(data), "content_hash": hex.EncodeToString(sum[:]),
			"client": "mekosh"})
	if err != nil {
		return uploadOffer{}, "", err
	}
	var o uploadOffer
	return o, reply, json.Unmarshal([]byte(reply), &o)
}

func (k cloudKB) status(ctx context.Context, key string) (fileState, string, error) {
	reply, err := k.t.cloudCall(ctx, http.MethodGet,
		cloudDatapackPath(k.dp, "/knowledge-bases/document-status?document_key="+url.QueryEscape(key)), nil)
	if err != nil {
		return fileState{}, "", err
	}
	var st fileState
	return st, reply, json.Unmarshal([]byte(reply), &st)
}

// knowledgeBase is the knowledge base of the datapack --datapack names.
func (t *tools) knowledgeBase(ctx context.Context, named string) (knowledgeBase, target, error) {
	dp, err := t.resolve(ctx, named)
	if err != nil || !dp.cloud {
		return localKB{t.toolSession, dp.id}, dp, err
	}
	return cloudKB{t, dp.id}, dp, nil
}

// statusHint is the upload_status line that asks how a file is doing.
func statusHint(dp target, key string) string {
	if dp.id == "" {
		return "mekosh upload_status " + key
	}
	return "mekosh upload_status " + key + " --datapack " + dp.spec()
}

// toolUpload uploads each file as an agent would: it asks the knowledge base for a link, posts the
// file there, and (unless --no-wait) follows its status until the file is indexed or failed. A file
// that fails makes the command fail, after the others are done.
func toolUpload(ctx context.Context, t *tools, f toolFlags, stdout io.Writer) error {
	if len(f.files) == 0 {
		return errors.New("give the files to upload: mekosh upload <file> [<file>...]")
	}
	kb, dp, err := t.knowledgeBase(ctx, f.datapack)
	if err != nil {
		return err
	}
	failed := 0
	for _, path := range f.files {
		if err := uploadOne(ctx, kb, dp, path, f, stdout); err != nil {
			failed++
			fmt.Fprintf(stdout, "%s: %v\n", path, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files weren't added", failed, len(f.files))
	}
	return nil
}

func uploadOne(ctx context.Context, kb knowledgeBase, dp target, path string, f toolFlags, stdout io.Writer) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err = knowledge.CheckSize(info.Size()); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), knowledge.ErrInvalid.Error()+": "))
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file the user named
	if err != nil {
		return err
	}
	offer, reply, err := kb.offer(ctx, filepath.Base(path), data)
	if err != nil {
		return err
	}
	if f.json {
		fmt.Fprintln(stdout, reply)
	}
	in := ""
	if dp.cloud {
		in = " in " + dp.noun()
	}
	// Bytes the datapack already has may still be queued, indexing, or failed and queued again: they
	// are waited for like a new upload.
	if offer.AlreadyUploaded {
		if !f.json {
			fmt.Fprintf(stdout, "%s: already in the knowledge base%s as %s (key %s)\n", path, in,
				offer.ExistingFilename, offer.Key)
		}
	} else if err = postUpload(ctx, offer.URL, offer.Fields, filepath.Base(path), data); err != nil {
		return err
	}
	if f.noWait {
		if !f.json && !offer.AlreadyUploaded {
			fmt.Fprintf(stdout, "%s: uploaded%s; %s says when it's indexed\n", path, in, statusHint(dp, offer.Key))
		}
		return nil
	}
	for {
		st, raw, err := kb.status(ctx, offer.Key)
		if err != nil {
			return err
		}
		switch st.State {
		case "completed", "failed", "abandoned", "oversized":
			if f.json {
				fmt.Fprintln(stdout, raw)
			} else if st.State == "completed" {
				fmt.Fprintf(stdout, "%s: indexed%s, %s (key %s)\n", path, in, chunks(st.Document.ChunksProcessed),
					offer.Key)
			}
			if st.State != "completed" {
				return errors.New(st.Message)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("still %s after %s: %s says how it's doing", st.State, uploadTimeout,
				statusHint(dp, offer.Key))
		case <-time.After(uploadPoll):
		}
	}
}

// postUpload posts a file to an upload link: its fields, then the file, as multipart/form-data.
func postUpload(ctx context.Context, url string, fields map[string]string, name string, data []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err = fw.Write(data); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	// G704: the upload link the running server or the linked Meko Cloud gave.
	resp, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// This Meko's link answers 204, as Meko Cloud's presigned S3 POST does by default; any 2xx is a
	// file received.
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	var env struct {
		Error *struct{ Message string } `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&env) == nil && env.Error != nil {
		return errors.New(env.Error.Message)
	}
	return fmt.Errorf("the upload answered %s", resp.Status)
}

// fileState is the part of knowledgebase_document_status's reply the CLI prints.
type fileState struct {
	State    string `json:"state"`
	Message  string `json:"message"`
	Document struct {
		ChunksProcessed int `json:"chunks_processed"`
	} `json:"document"`
}

func toolUploadStatus(ctx context.Context, t *tools, f toolFlags, stdout io.Writer) error {
	if f.text == "" {
		return errors.New("give the file's key: mekosh upload_status <key>")
	}
	kb, _, err := t.knowledgeBase(ctx, f.datapack)
	if err != nil {
		return err
	}
	st, raw, err := kb.status(ctx, f.text)
	if err != nil || f.json {
		fmt.Fprintln(stdout, raw)
		return err
	}
	fmt.Fprintf(stdout, "%s: %s. %s\n", f.text, st.State, st.Message)
	if st.Document.ChunksProcessed > 0 {
		fmt.Fprintf(stdout, "  %s indexed\n", chunks(st.Document.ChunksProcessed))
	}
	return nil
}

func chunks(n int) string {
	if n == 1 {
		return "1 chunk"
	}
	return fmt.Sprintf("%d chunks", n)
}
