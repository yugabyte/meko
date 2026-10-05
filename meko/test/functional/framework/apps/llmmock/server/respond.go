package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func unmarshalStrict(raw string, v any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any, headers map[string]string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	writeRaw(w, status, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), headers)
}

func writeRaw(w http.ResponseWriter, status int, data []byte, headers map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func sleepCtx(r *http.Request, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
