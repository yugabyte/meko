package cloudsync

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// S3's presigned POST answers a chunked body with 411 MissingContentLength, so the upload has to
// say its length up front, and the form it sends has to carry the fields and the whole file.
func TestPostFileSendsItsLength(t *testing.T) {
	file := bytes.Repeat([]byte("datapack "), 4096)
	var got []byte
	var fields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
			http.Error(w, "MissingContentLength", http.StatusLengthRequired)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: a test server
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got, _ = io.ReadAll(f)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := postFile(context.Background(), srv.URL, map[string]string{"key": "imports/x.zip", "policy": "p"},
		bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, file) || fields["key"] != "imports/x.zip" || fields["policy"] != "p" {
		t.Errorf("got %d of %d bytes, fields %v", len(got), len(file), fields)
	}
}
