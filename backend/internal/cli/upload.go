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
	"os"
	"path/filepath"
	"time"

	"github.com/yugabyte/meko/backend/internal/knowledge"
)

// uploadTimeout bounds meko upload: a 50 MiB file takes many minutes to index.
const uploadTimeout = time.Hour

// uploadPoll is how often meko upload asks how a file is doing.
const uploadPoll = time.Second

// toolUpload uploads each file as an agent would: it asks knowledgebase_upload_document for a link,
// posts the file there, and (unless --no-wait) follows knowledgebase_document_status until the
// file is indexed or failed. A file that fails makes the command fail, after the others are done.
func toolUpload(ctx context.Context, t *toolSession, f toolFlags, stdout io.Writer) error {
	if len(f.files) == 0 {
		return errors.New("give the files to upload: meko upload <file> [<file>...]")
	}
	dp, err := t.datapackID(ctx, f.datapack)
	if err != nil {
		return err
	}
	failed := 0
	for _, path := range f.files {
		if err := uploadOne(ctx, t, dp, path, f, stdout); err != nil {
			failed++
			fmt.Fprintf(stdout, "%s: %v\n", path, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files weren't added", failed, len(f.files))
	}
	return nil
}

func uploadOne(ctx context.Context, t *toolSession, dp, path string, f toolFlags, stdout io.Writer) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err = knowledge.CheckSize(info.Size()); err != nil {
		return errors.Unwrap(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file the user named
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	name := filepath.Base(path)
	reply, err := t.call(ctx, "knowledgebase_upload_document", map[string]any{"filename": name, "size": len(data),
		"content_hash": hex.EncodeToString(sum[:]), "datapack_id": dp,
		"content_head_base64": base64.StdEncoding.EncodeToString(data[:min(len(data), knowledge.SniffBytes)])})
	if err != nil {
		return err
	}
	if f.json {
		fmt.Fprintln(stdout, reply)
	}
	var offer struct {
		URL              string            `json:"url"`
		Fields           map[string]string `json:"fields"`
		Key              string            `json:"key"`
		AlreadyUploaded  bool              `json:"already_uploaded"`
		ExistingFilename string            `json:"existing_filename"`
	}
	if err = json.Unmarshal([]byte(reply), &offer); err != nil {
		return err
	}
	if offer.AlreadyUploaded {
		if !f.json {
			fmt.Fprintf(stdout, "%s: already in the knowledge base as %s (key %s)\n", path, offer.ExistingFilename,
				offer.Key)
		}
		return nil
	}
	if err = postUpload(ctx, offer.URL, offer.Fields, name, data); err != nil {
		return err
	}
	if f.noWait {
		if !f.json {
			fmt.Fprintf(stdout, "%s: uploaded; meko upload_status %s says when it's indexed\n", path, offer.Key)
		}
		return nil
	}
	for {
		st, raw, err := uploadStatus(ctx, t, dp, offer.Key)
		if err != nil {
			return err
		}
		switch st.State {
		case "completed", "failed":
			if f.json {
				fmt.Fprintln(stdout, raw)
			} else if st.State == "completed" {
				fmt.Fprintf(stdout, "%s: indexed, %s (key %s)\n", path, chunks(st.Document.ChunksProcessed), offer.Key)
			}
			if st.State == "failed" {
				return errors.New(st.Message)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("still %s after %s: meko upload_status %s says how it's doing", st.State,
				uploadTimeout, offer.Key)
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
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the running server's own upload link
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
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

func uploadStatus(ctx context.Context, t *toolSession, dp, key string) (fileState, string, error) {
	reply, err := t.call(ctx, "knowledgebase_document_status", map[string]any{"document_key": key, "datapack_id": dp})
	if err != nil {
		return fileState{}, reply, err
	}
	var st fileState
	return st, reply, json.Unmarshal([]byte(reply), &st)
}

func toolUploadStatus(ctx context.Context, t *toolSession, f toolFlags, stdout io.Writer) error {
	if f.text == "" {
		return errors.New("give the file's key: meko upload_status <key>")
	}
	dp, err := t.datapackID(ctx, f.datapack)
	if err != nil {
		return err
	}
	st, raw, err := uploadStatus(ctx, t, dp, f.text)
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
