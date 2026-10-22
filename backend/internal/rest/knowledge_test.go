package rest_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/testenv"
)

func str(v any) string { s, _ := v.(string); return s }

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// post posts a file to an upload link, with no token, as a browser posts one to S3.
func post(t *testing.T, link string, fields map[string]any, filename string, data []byte) int {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = w.WriteField(k, str(v))
	}
	fw, _ := w.CreateFormFile("file", filename)
	_, _ = fw.Write(data)
	_ = w.Close()
	resp, err := http.Post(link, w.FormDataContentType(), &body) //nolint:gosec,noctx // the test server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// uploadFile uploads a file through the presigned flow, as meko_ui does, and waits until it's indexed.
func uploadFile(t *testing.T, e *testenv.Env, name string, data []byte) map[string]any {
	t.Helper()
	kb := "/datapacks/" + e.DatapackID + "/knowledge-bases"
	offer := obj(okData(t, e, "POST", kb+"/upload-url", map[string]any{"filename": name, "size": len(data),
		"content_hash": sha(data), "client": "browser"}, 200))
	if offer["already_uploaded"] == true {
		return offer
	}
	if code := post(t, str(offer["url"]), obj(offer["fields"]), name, data); code != http.StatusNoContent {
		t.Fatalf("upload answered %d", code)
	}
	okData(t, e, "POST", kb+"/upload-complete", map[string]any{"key": offer["key"], "filename": name,
		"size": len(data)}, 200)
	okData(t, e, "POST", kb+"/create", nil, 201)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		st := obj(okData(t, e, "GET", kb+"/document-status?document_key="+url.QueryEscape(str(offer["key"])),
			nil, 200))
		if st["state"] == "completed" || st["state"] == "failed" {
			return offer
		}
	}
	t.Fatalf("%s was never indexed", name)
	return nil
}

func TestKnowledgeRoutes(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	kb := "/datapacks/" + e.DatapackID + "/knowledge-bases"
	page := []byte("<html><head><title>Onboarding</title></head><body><h1>Laptops</h1>" +
		"<p>New hires get a laptop on day one from the IT desk.</p></body></html>")
	offer := uploadFile(t, e, "onboarding.html", page)
	key := str(offer["key"])
	if offer["status_url"] == nil || offer["datapack_id"] != nil ||
		!strings.HasPrefix(key, e.UserID+"/"+e.DatapackID+"/") {
		t.Errorf("offer %v", offer)
	}

	// The status route, as the UI polls it.
	st := obj(okData(t, e, "GET", kb+"/status", nil, 200))
	src := arr(st["source_statuses"])
	det := arr(st["pipeline_details"])
	if len(src) != 1 || obj(src[0])["status"] != "COMPLETED" || len(det) != 1 ||
		obj(det[0])["document_name"] != key || obj(det[0])["document_status"] != "COMPLETED" {
		t.Errorf("status %v", st)
	}

	// The documents list, filtered as the UI filters it.
	list := obj(okData(t, e, "GET", kb+"/documents?type_filter=.html&q=onboard", nil, 200))
	items := arr(list["items"])
	if num(list["total"]) != 1 || list["has_more"] != false || len(items) != 1 {
		t.Fatalf("documents %v", list)
	}
	it := obj(items[0])
	if it["document_name"] != key || it["status"] != "COMPLETED" || num(it["chunks_processed"]) < 1 ||
		num(it["size_bytes"]) != float64(len(page)) || it["document_checksum"] != sha(page) ||
		it["document_uri"] != "local://"+key || it["uploaded_at"] == nil || it["completed_at"] == nil {
		t.Errorf("item %v", it)
	}
	if list = obj(okData(t, e, "GET", kb+"/documents?type_filter=.pdf", nil, 200)); num(list["total"]) != 0 {
		t.Errorf("a type filter: %v", list)
	}

	// The summary, in Cloud's words.
	sum := obj(okData(t, e, "GET", kb+"/summary", nil, 200))
	if num(sum["total_files"]) != 1 || num(sum["chunk_size"]) != 1000 || sum["embedding_model"] != "hash" ||
		!strings.HasPrefix(str(sum["files_summary"]), "1 file totaling") ||
		!strings.Contains(str(sum["description"]), "Heaviest source: onboarding.html.") {
		t.Errorf("summary %v", sum)
	}

	// The activity log, a page at a time.
	act := obj(okData(t, e, "GET", kb+"/activity-log?limit=1", nil, 200))
	acts := arr(act["items"])
	if len(acts) != 1 || obj(acts[0])["type"] != "file_upload" || obj(acts[0])["document_name"] != key ||
		act["has_more"] != true || act["next_cursor"] == "" {
		t.Errorf("activity %v", act)
	}
	act = obj(okData(t, e, "GET", kb+"/activity-log?limit=1&cursor="+str(act["next_cursor"]), nil, 200))
	if len(arr(act["items"])) != 0 || act["has_more"] != false || act["next_cursor"] != "" {
		t.Errorf("activity page 2 %v", act)
	}

	// Download: a link good once, sending the file as an attachment, HTML as plain text.
	dl := obj(okData(t, e, "POST", kb+"/download-url", map[string]any{"document_uri": "local://" + key}, 200))
	resp, err := http.Get(str(dl["url"])) //nolint:gosec,noctx // the test server
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, page) || num(dl["expires_in"]) != 300 ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("download: %d %q %v", resp.StatusCode, resp.Header, dl)
	}
	if resp, err = http.Get(str(dl["url"])); err == nil { //nolint:gosec,noctx // the test server
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a used download link answered %d", resp.StatusCode)
		}
	}
	wantErr(t, e, "POST", kb+"/download-url", map[string]any{"document_uri": "local://someone/else/x"}, 403, "FORBIDDEN")
	wantErr(t, e, "POST", kb+"/download-url", map[string]any{"document_uri": "s3://bucket/" + key}, 400,
		"VALIDATION_ERROR")

	// Uploads and their refusals.
	wantErr(t, e, "POST", kb+"/upload-url", map[string]any{"filename": "a.exe", "size": 3,
		"content_hash": sha([]byte("abc"))}, 400, "VALIDATION_ERROR")
	wantErr(t, e, "POST", kb+"/upload-url", map[string]any{"filename": "a.txt", "size": 0, "content_hash": sha(nil)},
		400, "VALIDATION_ERROR")
	again := obj(okData(t, e, "POST", kb+"/upload-url", map[string]any{"filename": "dup.html", "size": len(page),
		"content_hash": sha(page)}, 200))
	if again["already_uploaded"] != true || again["existing_filename"] != "onboarding.html" || again["url"] != "" {
		t.Errorf("dedupe %v", again)
	}
	wantErr(t, e, "POST", kb+"/upload-complete", map[string]any{"key": "x/y/z", "filename": "z"}, 403, "FORBIDDEN")
	wantErr(t, e, "POST", kb+"/upload-complete", map[string]any{"key": e.UserID + "/" + e.DatapackID + "/nope/z",
		"filename": "z"}, 404, "NOT_FOUND")
	wantErr(t, e, "GET", kb+"/document-status", nil, 400, "VALIDATION_ERROR")
	wantErr(t, e, "GET", kb+"/document-status?document_key=nope", nil, 404, "NOT_FOUND")

	// A waiting link can be canceled; a stored file is referenced.
	txt := []byte("a note waiting to be uploaded")
	waiting := obj(okData(t, e, "POST", kb+"/upload-url", map[string]any{"filename": "note.txt", "size": len(txt),
		"content_hash": sha(txt)}, 200))
	wkey := str(waiting["key"])
	if p := obj(okData(t, e, "GET", kb+"/pending-uploads", nil, 200)); num(p["count"]) != 0 {
		t.Errorf("pending %v", p)
	}
	okData(t, e, "DELETE", kb+"/pending-uploads?key="+url.QueryEscape(wkey), nil, 200)
	if code := post(t, str(waiting["url"]), nil, "note.txt", txt); code != http.StatusForbidden {
		t.Errorf("a canceled link answered %d", code)
	}
	wantErr(t, e, "DELETE", kb+"/pending-uploads?key="+url.QueryEscape(key), nil, 409, "OBJECT_REFERENCED")

	// The sink takes a file up to 50 MiB, past /api/v1's 1 MB limit; a token-less request elsewhere is 401.
	big := bytes.Repeat([]byte("knowledge "), 200_000) // 2 MB
	bigOffer := uploadFile(t, e, "big.txt", big)
	if bigOffer["key"] == nil {
		t.Fatal("no key for the big file")
	}
	resp, err = http.Post(e.URL+"/api/v1"+kb+"/upload-url", "application/json", strings.NewReader("{}")) //nolint:noctx
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("upload-url without a token answered %d", resp.StatusCode)
		}
	}

	// Metrics count the knowledge base.
	mt := obj(okData(t, e, "GET", "/datapacks/"+e.DatapackID+"/metrics", nil, 200))
	if num(mt["kb_file_count"]) != 2 || num(mt["kb_chunk_count"]) < 2 ||
		num(mt["kb_bytes"]) != float64(len(page)+len(big)) {
		t.Errorf("metrics %v", mt)
	}

	// Delete, by the document's id.
	id := str(it["id"])
	del := obj(okData(t, e, "DELETE", kb+"/documents/"+id, nil, 200))
	if del["document_id"] != id || num(del["chunks_deleted"]) < 1 || del["s3_deleted"] != true {
		t.Errorf("delete %v", del)
	}
	wantErr(t, e, "DELETE", kb+"/documents/"+id, nil, 404, "NOT_FOUND")
	wantErr(t, e, "DELETE", kb+"/documents/not-a-uuid", nil, 404, "NOT_FOUND")
}

// AgentK's upload (extensions/agentk/src/server/mekoapi.ts requestUpload, postPresigned and
// documentStatus, since #225), replayed as it sends it: upload-url with the client token and its
// client name, the fields then the file to the link with no token, then document-status until its
// state is final. A change to those calls updates this replay.
func TestAgentKUpload(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	kb := "/datapacks/" + e.DatapackID + "/knowledge-bases"
	file := []byte("# Deploys\n\nProduction deploys need two approvals from the release team.\n")
	offer := obj(okData(t, e, "POST", kb+"/upload-url", map[string]any{"filename": "deploys.md", "size": len(file),
		"content_hash": sha(file), "client": "agent-k-local"}, 200))
	key, link := str(offer["key"]), str(offer["url"])
	fields := obj(offer["fields"])
	if key == "" || link == "" || fields == nil || offer["already_uploaded"] != false ||
		!strings.HasPrefix(link, "http://127.0.0.1:") {
		t.Fatalf("offer %v", offer)
	}
	if code := post(t, link, fields, "deploys.md", file); code < 200 || code > 299 {
		t.Fatalf("the link answered %d", code)
	}
	state := ""
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		st := obj(okData(t, e, "GET", kb+"/document-status?document_key="+url.QueryEscape(key), nil, 200))
		if state = strings.ToLower(str(st["state"])); state == "completed" || state == "failed" {
			break
		}
	}
	if state != "completed" {
		t.Fatalf("state %q", state)
	}
	// The same bytes again: already_uploaded with the key, and nothing to post.
	again := obj(okData(t, e, "POST", kb+"/upload-url", map[string]any{"filename": "deploys.md", "size": len(file),
		"content_hash": sha(file), "client": "agent-k-local"}, 200))
	if again["already_uploaded"] != true || str(again["key"]) != key {
		t.Errorf("again %v", again)
	}
	docs := obj(okData(t, e, "GET", kb+"/documents", nil, 200))
	if items := arr(docs["items"]); len(items) != 1 || obj(items[0])["uploaded_by_user_id"] != e.UserID {
		t.Errorf("documents %v", docs)
	}
}
