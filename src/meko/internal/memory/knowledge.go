package memory

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/knowledge"
	"github.com/yugabyte/meko/src/meko/internal/memoryengine"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// Knowledge-base search follows Meko Cloud's hybrid search.
const (
	// kbPool is the fewest candidates each of the vector and keyword legs takes; a larger limit
	// takes more. As in Cloud, limit widens the pool and doesn't cap the reply.
	kbPool = 20
	// maxReranked is how many of the best fused candidates the cross-encoder scores: about 11 ms a
	// pair of a query and a 1000-character chunk on 8 cores, so 20 take about 260 ms (40, Cloud's
	// most, took 440).
	maxReranked = 20
	// trimRatio trims the reranked results as Cloud does: a result stays while its score is at least
	// this share of the one before and of the best.
	trimRatio = 0.3
	// rrfK is reciprocal-rank fusion's constant, which orders the candidates before reranking.
	rrfK = 60
	// DefaultKBMaxDistance is the farthest (cosine distance) a knowledge-base result found by meaning
	// alone may be from the query for context_search to keep it (keptKB keeps reranked word matches
	// past it). Cloud's 0.5 suits its embedding model; with
	// all-MiniLM-L6-v2 a paraphrased question measured 0.37 to 0.51 from the chunk that answered it,
	// and unrelated chunks 0.87 to 0.99.
	DefaultKBMaxDistance = 0.6
	// chunkInsertBatch is how many chunks one write transaction stores, so indexing a large file
	// never holds the writer for long.
	chunkInsertBatch = 500
	// indexSweep is how often the indexer looks for queued files it wasn't told about.
	indexSweep = 30 * time.Second
	// ticketTTL is how long an upload or download link works, as Cloud's presigned URLs.
	ticketTTL = 5 * time.Minute
	// IndexName names the knowledge base's index, as Cloud's replies do.
	IndexName = "datapacks.knowledge_bases"
)

// Paths of the ticket routes, which the server serves without a token: the ticket in them is the
// credential, as a presigned URL's signature is.
const (
	UploadTicketPath   = "/api/v1/knowledge-uploads/"
	DownloadTicketPath = "/api/v1/knowledge-downloads/"
)

// Errors the knowledge-file calls return.
var (
	// ErrTicket is an upload or download link that's unknown, used or expired.
	ErrTicket = errors.New("the link is unknown, used or expired; ask for a new one")
	// ErrForeignKey is a document key outside the datapack.
	ErrForeignKey = errors.New("the key isn't in this datapack")
	// ErrDocumentProcessing is a file that can't be deleted while it's queued or being indexed.
	ErrDocumentProcessing = errors.New("the file is still being indexed; delete it once it's done")
	// ErrReferenced is an upload link whose file is already stored.
	ErrReferenced = errors.New("the file is stored; delete it as a document")
)

// ticket is an upload or download link's: what it may do, until when.
type ticket struct {
	upload   bool
	datapack string
	key      string
	filename string
	docType  string
	hash     string
	size     int64
	hold     int64 // the bytes an upload link holds against the storage limit until its file is stored
	agent    string
	expires  time.Time
}

// tickets are the links handed out, in memory: a restart voids them, as their 5 minutes would.
type tickets struct {
	mu sync.Mutex
	m  map[string]*ticket
	// receiving is what the upload links being used hold, from when their file starts arriving until
	// it's stored or refused (received).
	receiving int64
}

// held is the bytes upload links hold against the storage limit: the live ones', each file once
// however many links were asked for it, and the ones whose files are arriving.
func (t *tickets) held() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	byFile, now := map[string]int64{}, time.Now()
	for _, tk := range t.m {
		if f := tk.datapack + "/" + tk.hash; tk.upload && now.Before(tk.expires) {
			byFile[f] = max(byFile[f], tk.hold)
		}
	}
	n := t.receiving
	for _, h := range byFile {
		n += h
	}
	return n
}

// holding says whether a live upload link already holds a datapack's file (by its SHA-256).
func (t *tickets) holding(datapack, hash string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for _, tk := range t.m {
		if tk.upload && tk.hold > 0 && tk.datapack == datapack && tk.hash == hash && now.Before(tk.expires) {
			return true
		}
	}
	return false
}

// received ends what an upload link taken by take holds.
func (t *tickets) received(tk *ticket) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.receiving -= tk.hold
}

func (t *tickets) add(tk *ticket) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	now := time.Now()
	tk.expires = now.Add(ticketTTL)
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.m {
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[token] = tk
	return token, nil
}

// take uses up a link of the kind asked for; ok is false when there's none, or it expired.
func (t *tickets) take(token string, upload bool) (*ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[token]
	if !ok || tk.upload != upload {
		return nil, false
	}
	delete(t.m, token)
	live := time.Now().Before(tk.expires)
	if live && upload {
		t.receiving += tk.hold // until received: the file's bytes are arriving
	}
	return tk, live
}

// waiting is the live upload link for a datapack's key, if any.
func (t *tickets) waiting(datapack, key string) *ticket {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for _, tk := range t.m {
		if tk.upload && tk.datapack == datapack && tk.key == key && now.Before(tk.expires) {
			return tk
		}
	}
	return nil
}

// cancel voids the upload links for a datapack's key and says whether there were any.
func (t *tickets) cancel(datapack, key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	found := false
	for k, tk := range t.m {
		if tk.upload && tk.datapack == datapack && tk.key == key {
			delete(t.m, k)
			found = true
		}
	}
	return found
}

// documentKey is a file's key, as Cloud keys its objects: <user>/<datapack>/<sha256>/<name>.
func (s *Service) documentKey(datapack, hash, filename string) string {
	return s.keyPrefix(datapack) + hash + "/" + knowledge.SanitizeFilename(filename)
}

func (s *Service) keyPrefix(datapack string) string { return s.userID + "/" + datapack + "/" }

// SourceURI is a datapack's knowledge base, as Cloud names its source (an S3 prefix there).
func (s *Service) SourceURI(datapack string) string { return "local://" + s.keyPrefix(datapack) }

// DocumentURI is a file's URI, as download-url takes it.
func DocumentURI(key string) string { return "local://" + key }

// URL is a path on this server, absolute.
func (s *Service) URL(path string) string { return s.baseURL + path }

// UploadRequest asks for a link to upload a file to, as Cloud's upload-url does.
type UploadRequest struct {
	DatapackID  string
	Filename    string
	Size        int64
	ContentHash string // the SHA-256 of its bytes, hex
	AgentID     string // who uploads it
	// Head, if set, is the file's first bytes (up to knowledge.SniffBytes), checked against its type
	// before a link is given, as Cloud's knowledgebase_upload_document checks them.
	Head []byte
}

// UploadOffer is where to upload a file, or the file already there.
type UploadOffer struct {
	DatapackID       string
	URL              string // "" when the file is already uploaded
	Fields           map[string]string
	Key              string
	AlreadyUploaded  bool
	ExistingFilename string
	StatusURL        string
	ExpiresAt        time.Time
}

// UploadURL checks a file's name, size and hash and gives a link to POST it to (multipart, its
// fields first and the file last), good once for 5 minutes. When the datapack already has these
// bytes there's nothing to upload: it says so, and queues them again if they FAILED.
func (s *Service) UploadURL(ctx context.Context, r UploadRequest) (UploadOffer, error) {
	done, werr := s.writing(r.DatapackID)
	if werr != nil {
		return UploadOffer{}, werr
	}
	defer done()
	if err := s.needDatapack(ctx, r.DatapackID); err != nil {
		return UploadOffer{}, err
	}
	docType, err := knowledge.TypeOf(r.Filename)
	if err != nil {
		return UploadOffer{}, err
	}
	if err = knowledge.CheckSize(r.Size); err != nil {
		return UploadOffer{}, err
	}
	hash := strings.ToLower(strings.TrimSpace(r.ContentHash))
	if !knowledge.IsHash(hash) {
		return UploadOffer{}, fmt.Errorf("%w: content_hash must be the file's SHA-256, 64 hex characters",
			knowledge.ErrInvalid)
	}
	if r.Head != nil {
		if want := min(r.Size, knowledge.SniffBytes); int64(len(r.Head)) < want || int64(len(r.Head)) > r.Size {
			return UploadOffer{}, fmt.Errorf("%w: send the file's first %d bytes (all of it if it's smaller)",
				knowledge.ErrInvalid, knowledge.SniffBytes)
		}
		if err = knowledge.Sniff(r.Filename, r.Head); err != nil {
			return UploadOffer{}, err
		}
	}
	status := func(key string) string {
		return s.URL(fmt.Sprintf("/api/v1/datapacks/%s/knowledge-bases/document-status?document_key=%s",
			r.DatapackID, url.QueryEscape(key)))
	}
	existing, err := s.st.DocumentByHash(ctx, r.DatapackID, hash)
	key := s.documentKey(r.DatapackID, hash, r.Filename)
	hold := r.Size
	switch {
	case err == nil && existing.Status == store.DocFailed && !s.files.Has(r.DatapackID, hash):
		// It failed because its bytes are gone: they're needed again, under its key. Once they
		// arrive, the document is queued again (AddDocument requeues a FAILED one). Its row already
		// counts its size, so its link holds nothing more.
		key, hold = existing.Key, 0
	case err == nil:
		if existing.Status == store.DocFailed {
			if _, _, err = s.st.AddDocument(ctx, existing); err != nil {
				return UploadOffer{}, err
			}
			s.kickIndexer()
		}
		return UploadOffer{DatapackID: r.DatapackID, Key: existing.Key, AlreadyUploaded: true,
			ExistingFilename: existing.Filename, StatusURL: status(existing.Key)}, nil
	case !errors.Is(err, store.ErrNotFound):
		return UploadOffer{}, err
	}
	// A file a live link already holds (a retried upload-url, or the same file under another name) needs
	// no more room: its links hold its size once.
	release := func() {}
	if hold > 0 && !s.tickets.holding(r.DatapackID, hash) {
		if release, err = s.reserve(ctx, hold); err != nil {
			return UploadOffer{}, err
		}
	}
	tk := &ticket{upload: true, datapack: r.DatapackID, key: key, filename: r.Filename, docType: docType, hash: hash,
		size: r.Size, hold: hold, agent: r.AgentID}
	token, err := s.tickets.add(tk)
	release() // the link holds the size now
	if err != nil {
		return UploadOffer{}, err
	}
	return UploadOffer{DatapackID: r.DatapackID, URL: s.URL(UploadTicketPath + token),
		Fields: map[string]string{"key": key}, Key: key, StatusURL: status(key), ExpiresAt: tk.expires}, nil
}

// ReceiveUpload takes the file an upload link was made for: its bytes must be the size and SHA-256
// the link was asked with, and look like its type. They're stored and queued for indexing at once,
// as Cloud's watcher registers a file nobody confirms. A link works once: a refused file spends it.
func (s *Service) ReceiveUpload(ctx context.Context, token string, r io.Reader) error {
	tk, ok := s.tickets.take(token, true)
	if !ok {
		return ErrTicket
	}
	done, werr := s.writing(tk.datapack)
	if werr != nil {
		s.tickets.received(tk)
		return werr
	}
	defer done()
	// Its size stays held until the document's row counts it, or the file is refused.
	defer s.tickets.received(tk)
	rec, err := s.files.Receive(tk.datapack, r, tk.size)
	if errors.Is(err, knowledge.ErrTooLarge) {
		return fmt.Errorf("%w: the file is larger than the %d bytes its upload link was asked for",
			knowledge.ErrInvalid, tk.size)
	}
	if err != nil {
		return err
	}
	refuse := func(e error) error {
		s.files.Discard(rec)
		return e
	}
	switch {
	case rec.Size != tk.size:
		return refuse(fmt.Errorf("%w: the file is %d bytes, not the %d its upload link was asked for",
			knowledge.ErrInvalid, rec.Size, tk.size))
	case rec.Hash != tk.hash:
		return refuse(fmt.Errorf("%w: the file's SHA-256 isn't the content_hash its upload link was asked for",
			knowledge.ErrInvalid))
	}
	if err = knowledge.Sniff(tk.filename, rec.Head); err != nil {
		return refuse(err)
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if err = s.files.Keep(tk.datapack, rec); err != nil {
		return refuse(err)
	}
	_, _, err = s.st.AddDocument(ctx, store.Document{DatapackID: tk.datapack, Key: tk.key, Filename: tk.filename,
		DocumentType: tk.docType, ContentHash: tk.hash, SizeBytes: tk.size, UploadedBy: tk.agent})
	if err != nil {
		// Without its row the file is an orphan, unless a row for the same bytes already names it.
		if has, herr := s.st.HasDocumentFile(context.WithoutCancel(ctx), tk.datapack, tk.hash); herr == nil && !has {
			_ = s.files.Remove(tk.datapack, tk.hash)
		}
		if errors.Is(err, store.ErrNoDatapack) {
			return ErrNoDatapack
		}
		return err
	}
	s.kickIndexer()
	return nil
}

// UploadLocalFile uploads a file from this machine's disk in one step, the server reading it
// itself: the same checks, storing and queueing as an upload link's, with no bytes on the wire.
// It reads as the server's user, so callers must hold that user's token (the MCP tool checks).
func (s *Service) UploadLocalFile(ctx context.Context, datapack, agent, path string) (UploadOffer, error) {
	lf, err := knowledge.OpenLocal(path)
	if err != nil {
		return UploadOffer{}, err
	}
	defer func() { _ = lf.File.Close() }()
	offer, err := s.UploadURL(ctx, UploadRequest{DatapackID: datapack, Filename: lf.Name, Size: lf.Size,
		ContentHash: lf.Hash, AgentID: agent, Head: lf.Head})
	if err != nil || offer.AlreadyUploaded {
		return offer, err
	}
	if _, err = lf.File.Seek(0, io.SeekStart); err != nil {
		return UploadOffer{}, err
	}
	if err = s.ReceiveUpload(ctx, strings.TrimPrefix(offer.URL, s.URL(UploadTicketPath)), lf.File); err != nil {
		return UploadOffer{}, err
	}
	return offer, nil
}

// DocumentStatus is a file's indexing state, by its key: its document once its bytes are stored,
// or the upload link still waiting for them.
type DocumentStatus struct {
	Key      string
	Filename string
	Document *store.Document
	// Waiting, while the bytes haven't arrived: when the link expires.
	WaitingUntil *time.Time
}

// DocumentStatus says how a file is doing, or ErrNotFound for a key neither stored nor waiting.
func (s *Service) DocumentStatus(ctx context.Context, datapack, key string) (DocumentStatus, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return DocumentStatus{}, err
	}
	d, err := s.st.DocumentByKey(ctx, datapack, key)
	if err == nil {
		return DocumentStatus{Key: key, Filename: d.Filename, Document: &d}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return DocumentStatus{}, err
	}
	if tk := s.tickets.waiting(datapack, key); tk != nil {
		exp := tk.expires
		return DocumentStatus{Key: key, Filename: tk.filename, WaitingUntil: &exp}, nil
	}
	return DocumentStatus{}, ErrNotFound
}

// ConfirmUpload is upload-complete: the key must be the datapack's and its file stored. Files are
// registered as they arrive, so there's nothing more to do.
func (s *Service) ConfirmUpload(ctx context.Context, datapack, key string) error {
	done, werr := s.writing(datapack)
	if werr != nil {
		return werr
	}
	defer done()
	if err := s.needDatapack(ctx, datapack); err != nil {
		return err
	}
	if !strings.HasPrefix(key, s.keyPrefix(datapack)) {
		return ErrForeignKey
	}
	if _, err := s.st.DocumentByKey(ctx, datapack, key); errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// CreateKnowledgeBase is create: it marks the datapack as having a knowledge base. Files are
// registered and queued as they arrive, so nothing is left to register.
func (s *Service) CreateKnowledgeBase(ctx context.Context, datapack string) error {
	done, werr := s.writing(datapack)
	if werr != nil {
		return werr
	}
	defer done()
	err := s.st.MarkKnowledgeBase(ctx, datapack)
	if errors.Is(err, store.ErrNoDatapack) {
		return ErrNoDatapack
	}
	return err
}

// CancelUpload voids a key's waiting upload link. A key whose file is stored is ErrReferenced.
func (s *Service) CancelUpload(ctx context.Context, datapack, key string) error {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return err
	}
	if !strings.HasPrefix(key, s.keyPrefix(datapack)) {
		return ErrForeignKey
	}
	if _, err := s.st.DocumentByKey(ctx, datapack, key); err == nil {
		return ErrReferenced
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.tickets.cancel(datapack, key)
	return nil
}

// Documents is a page of a datapack's files, newest first, and how many match in all. An unknown
// status filter is ignored, as Cloud ignores it.
func (s *Service) Documents(ctx context.Context, p store.DocumentPage) ([]store.Document, int, error) {
	if err := s.needDatapack(ctx, p.DatapackID); err != nil {
		return nil, 0, err
	}
	switch p.Status {
	case store.DocQueued, store.DocProcessing, store.DocCompleted, store.DocFailed:
	case "NOT_STARTED", "RETRY":
		return []store.Document{}, 0, nil // statuses Meko never sets
	default:
		p.Status = ""
	}
	return s.st.ListDocuments(ctx, p)
}

// KnowledgeStats sums up a datapack's knowledge base.
func (s *Service) KnowledgeStats(ctx context.Context, datapack string) (store.KBStats, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return store.KBStats{}, err
	}
	return s.st.KnowledgeStats(ctx, datapack)
}

// Embedder is the embedding model the knowledge base's vectors were made with, if known.
func (s *Service) Embedder(ctx context.Context) (store.Embedder, bool, error) {
	return s.st.Embedder(ctx)
}

// Activity is a page of a datapack's file uploads and promotions, newest first, after cursor (""
// for the newest; one that can't be read is the newest too, as in Cloud), and the next page's
// cursor ("" when this page is the last).
func (s *Service) Activity(ctx context.Context, datapack, cursor string, limit int) ([]store.Activity, string, error) {
	if err := s.needDatapack(ctx, datapack); err != nil {
		return nil, "", err
	}
	return activityPage(cursor, limit, func(after *store.ActivityCursor) ([]store.Activity, *store.ActivityCursor,
		error) {
		return s.st.KBActivity(ctx, datapack, after, limit)
	})
}

// activityPage reads a page of activity from the opaque cursor a page before gave ("" for the
// newest), and gives the next page's ("" when there's none).
func activityPage(cursor string, limit int,
	read func(after *store.ActivityCursor) ([]store.Activity, *store.ActivityCursor, error)) (
	[]store.Activity, string, error) {
	var after *store.ActivityCursor
	if b, err := base64.RawURLEncoding.DecodeString(cursor); cursor != "" && err == nil {
		var c store.ActivityCursor
		if json.Unmarshal(b, &c) == nil && c.Timestamp != "" {
			after = &c
		}
	}
	items, last, err := read(after)
	if err != nil || len(items) < limit || last == nil {
		return items, "", err
	}
	b, err := json.Marshal(last)
	return items, base64.RawURLEncoding.EncodeToString(b), err
}

// DownloadURL gives a link to download a datapack's file by its URI (local://<key>), good once for
// 5 minutes, and the file's key.
func (s *Service) DownloadURL(ctx context.Context, datapack, uri string) (link, key string, err error) {
	if err = s.needDatapack(ctx, datapack); err != nil {
		return "", "", err
	}
	key, ok := strings.CutPrefix(uri, "local://")
	if !ok {
		return "", "", fmt.Errorf("%w: document_uri must be a local:// URI, as the documents list gives it",
			ErrBadRequest)
	}
	if !strings.HasPrefix(key, s.keyPrefix(datapack)) {
		return "", "", ErrForeignKey
	}
	d, err := s.st.DocumentByKey(ctx, datapack, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	token, err := s.tickets.add(&ticket{datapack: datapack, key: key, filename: d.Filename, docType: d.DocumentType,
		hash: d.ContentHash})
	if err != nil {
		return "", "", err
	}
	return s.URL(DownloadTicketPath + token), key, nil
}

// Download is a file a download link was made for: open, with its name and type.
type Download struct {
	File     *os.File
	Filename string
	Type     string
}

// OpenDownload opens the file a download link was made for, or returns ErrTicket. The caller
// closes it.
func (s *Service) OpenDownload(token string) (Download, error) {
	tk, ok := s.tickets.take(token, false)
	if !ok {
		return Download{}, ErrTicket
	}
	f, err := s.files.Open(tk.datapack, tk.hash)
	if errors.Is(err, os.ErrNotExist) {
		return Download{}, ErrNotFound
	}
	return Download{File: f, Filename: tk.filename, Type: tk.docType}, err
}

// DeleteDocument deletes a datapack's file, its chunks and its bytes, and says how many chunks
// went, or returns ErrNotFound or ErrDocumentProcessing.
func (s *Service) DeleteDocument(ctx context.Context, datapack, id string) (int, error) {
	done, werr := s.writing(datapack)
	if werr != nil {
		return 0, werr
	}
	defer done()
	if err := s.needDatapack(ctx, datapack); err != nil {
		return 0, err
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	d, n, err := s.st.DeleteDocument(ctx, datapack, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 0, ErrNotFound
	case errors.Is(err, store.ErrDocumentProcessing):
		return 0, ErrDocumentProcessing
	case err != nil:
		return 0, err
	}
	if s.keepDeleted != nil && s.keepDeleted() {
		return n, nil // SweepFiles removes the bytes at a start after the update is done
	}
	if err = s.files.Remove(datapack, d.ContentHash); err != nil && s.log != nil {
		s.log.Warn("couldn't remove a deleted knowledge file's bytes; the next start sweeps them",
			"document_id", d.ID, "error", err)
	}
	return n, nil
}

// SweepFiles removes the files folder's files no document names (a crash between storing a file
// and its row, or a datapack deleted while its files were removed) and says how many it removed.
// serve runs it at start.
func (s *Service) SweepFiles(ctx context.Context) (int, error) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	return s.files.Sweep(func(datapack, hash string) (bool, error) {
		return s.st.HasDocumentFile(ctx, datapack, hash)
	})
}

// kickIndexer wakes the indexer without waiting for it.
func (s *Service) kickIndexer() {
	select {
	case s.indexKick <- struct{}{}:
	default:
	}
}

// RunIndexer indexes queued knowledge files until ctx ends: whenever one arrives, and every
// indexSweep for any it missed. serve runs it, after RequeueInterrupted.
func (s *Service) RunIndexer(ctx context.Context) {
	tick := time.NewTicker(indexSweep)
	defer tick.Stop()
	for {
		if err := s.IndexQueued(ctx); err != nil && ctx.Err() == nil && s.log != nil {
			s.log.Warn("can't index knowledge files; retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.indexKick:
		case <-tick.C:
		}
	}
}

// RequeueInterrupted puts back in the queue the files a stopped server was indexing.
func (s *Service) RequeueInterrupted(ctx context.Context) (int, error) {
	return s.st.RequeueProcessing(ctx)
}

// IndexQueued indexes every queued file, oldest first: it extracts and chunks each and stores its
// chunks for the embedder sweep, which completes it once they're all embedded.
func (s *Service) IndexQueued(ctx context.Context) error {
	for {
		d, err := s.st.ClaimDocument(ctx)
		if err != nil || d == nil {
			return err
		}
		if err = s.index(ctx, *d); err != nil {
			return err
		}
	}
}

// index extracts and chunks one claimed file. A file that can't be indexed is FAILED with the
// reason, so it never stays PROCESSING (which only a restart would put back in the queue); only a
// stop (ctx ending) leaves it for the next start to queue again.
func (s *Service) index(ctx context.Context, d store.Document) error {
	start := time.Now()
	fail := func(reason string) error {
		if s.log != nil {
			s.log.Info("couldn't index a knowledge file", "document_id", d.ID, "reason", reason)
		}
		return s.st.FailDocument(context.WithoutCancel(ctx), d.Rowid, reason)
	}
	// An error of Meko's own (a disk or database failure): logged, and the file failed with a reason
	// the user can act on, since uploading it again queues it again.
	failInternal := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err() // stopping: the next start queues it again
		}
		if s.log != nil {
			s.log.Warn("couldn't index a knowledge file", "document_id", d.ID, "error", err)
		}
		return fail("indexing it failed; upload it again to retry")
	}
	data, err := s.files.Read(d.DatapackID, d.ContentHash)
	if errors.Is(err, os.ErrNotExist) {
		return fail("the file's bytes are missing; upload it again")
	}
	if err != nil {
		return failInternal(err)
	}
	ectx, cancel := context.WithTimeout(ctx, knowledge.Deadline(d.SizeBytes))
	defer cancel()
	chunks, err := knowledge.Chunks(ectx, d.Filename, data)
	switch {
	case ctx.Err() != nil:
		return ctx.Err() // stopping: the next start queues it again
	case errors.Is(err, context.DeadlineExceeded):
		return fail("reading the file took too long")
	case errors.Is(err, knowledge.ErrNoText):
		return fail("the file has no text to index")
	case errors.Is(err, knowledge.ErrInvalid):
		return fail(strings.TrimPrefix(err.Error(), knowledge.ErrInvalid.Error()+": "))
	case err != nil:
		return failInternal(err)
	}
	for i := 0; i < len(chunks); i += chunkInsertBatch {
		end := min(i+chunkInsertBatch, len(chunks))
		err = s.st.AddChunks(ctx, d, i, chunks[i:end], end == len(chunks))
		if errors.Is(err, store.ErrNotFound) {
			return nil // deleted meanwhile, with its datapack
		}
		if err != nil {
			return failInternal(err) // FailDocument drops the chunks already stored
		}
		s.kickEmbedder()
	}
	if s.log != nil {
		s.log.Info("chunked a knowledge file", "document_id", d.ID, "chunks", len(chunks),
			"ms", time.Since(start).Milliseconds())
	}
	return nil
}

// embedKnowledgeBatch embeds up to embedBatch knowledge-base chunks, behind any search's queries,
// and says how many.
func (s *Service) embedKnowledgeBatch(ctx context.Context) (int, error) {
	pending, err := s.st.PendingKBChunks(ctx, embedBatch)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	texts := make([]string, len(pending))
	ids := make([]int64, len(pending))
	for i, c := range pending {
		texts[i], ids[i] = c.Text, c.Rowid
	}
	vecs, err := s.eng.EmbedBackground(ctx, texts)
	if err != nil {
		return 0, s.memoryEngineErr(err)
	}
	return len(pending), s.st.PutKBChunkVectors(ctx, ids, vecs)
}

// KBResult is a knowledge-base search result in Cloud's shape: a file's chunk, or a promoted memory
// (metadata_filters.source "memory", with its memory_id, and its conversation as document_id).
type KBResult struct {
	ID              string         `json:"id"`
	ChunkText       string         `json:"chunk_text"`
	DocumentID      string         `json:"document_id"`
	MetadataFilters map[string]any `json:"metadata_filters"`
	Distance        float64        `json:"distance"`
	MatchType       string         `json:"match_type"` // semantic, keyword or hybrid
	// RerankScore is the cross-encoder's 0-1 score; absent when the reranker couldn't run, as in Cloud.
	RerankScore *float64 `json:"rerank_score,omitempty"`
	Rank        *float64 `json:"rank,omitempty"` // a keyword or hybrid match's BM25 score
}

// SearchKnowledge searches a datapack's knowledge base as Meko Cloud does:
//   - The nearest max(20, limit) chunks by cosine, and as many by keyword (BM25), each with its
//     distance, merged and tagged semantic, keyword or hybrid.
//   - The cross-encoder scores the best of them (by reciprocal-rank fusion; at most
//     max(maxReranked, limit)), sigmoid-mapped to 0-1, best first.
//   - Results stay while each scores at least trimRatio of the one before and of the best, and at
//     least one stays.
//
// limit widens the pool but doesn't cap the reply, as in Cloud. If the reranker can't run, the
// candidates come back in fused order, untrimmed, without a rerank score, as Cloud falls back.
func (s *Service) SearchKnowledge(ctx context.Context, datapack, query string, limit int) ([]KBResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query must not be empty", ErrBadRequest)
	}
	if err := s.needDatapack(ctx, datapack); err != nil {
		return nil, err
	}
	vecs, err := s.eng.Embed(ctx, []string{query})
	if err != nil {
		return nil, s.memoryEngineErr(err)
	}
	pool := max(kbPool, limit)
	semantic, err := s.st.SearchKBVectors(ctx, datapack, vecs[0], pool)
	if err != nil {
		return nil, err
	}
	keyword, err := s.st.SearchKBKeywords(ctx, datapack, query, vecs[0], pool)
	if err != nil {
		return nil, err
	}
	cands := fuse(semantic, keyword)
	if len(cands) == 0 {
		return []KBResult{}, nil
	}
	top := cands[:min(len(cands), max(maxReranked, limit), memoryengine.MaxRerank)]
	texts := make([]string, len(top))
	for i, c := range top {
		texts[i] = c.ChunkText
	}
	scores, err := s.eng.Rerank(ctx, query, texts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if s.log != nil {
			if errors.Is(err, memoryengine.ErrRerankerNotReady) { // loading, or reviving a dead worker
				s.log.Debug("knowledge-base results are unranked: the reranker isn't ready", "error", err)
			} else {
				s.log.Warn("couldn't rerank knowledge-base results; returning them unranked", "error", err)
			}
		}
		return cands, nil
	}
	for i := range top {
		v := 1 / (1 + math.Exp(-scores[i]))
		top[i].RerankScore = &v
	}
	sort.SliceStable(top, func(i, j int) bool { return *top[i].RerankScore > *top[j].RerankScore })
	return trimResults(top), nil
}

// fuse merges the vector and keyword candidates, ordered by reciprocal-rank fusion (ties by
// distance). A chunk both found is a hybrid match and adds both ranks' shares, so it usually comes
// first; with a large pool (limit past about 60), a hybrid ranked low in both lists can fall below
// a single list's best.
func fuse(semantic, keyword []store.KBHit) []KBResult {
	type cand struct {
		r     KBResult
		fused float64
	}
	byRow := map[int64]*cand{}
	var order []*cand
	add := func(h store.KBHit, i int, kind string) {
		score := 1.0 / float64(rrfK+i+1)
		if c, ok := byRow[h.Rowid]; ok {
			c.fused += score
			c.r.MatchType, c.r.Rank = "hybrid", h.Rank
			return
		}
		c := &cand{r: KBResult{ID: h.ID, ChunkText: h.Text, DocumentID: h.DocumentID, MetadataFilters: h.Metadata,
			Distance: h.Distance, MatchType: kind, Rank: h.Rank}, fused: score}
		if c.r.MetadataFilters == nil {
			c.r.MetadataFilters = map[string]any{}
		}
		byRow[h.Rowid] = c
		order = append(order, c)
	}
	for i, h := range semantic {
		add(h, i, "semantic")
	}
	for i, h := range keyword {
		add(h, i, "keyword")
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].fused != order[j].fused {
			return order[i].fused > order[j].fused
		}
		return order[i].r.Distance < order[j].r.Distance
	})
	out := make([]KBResult, len(order))
	for i, c := range order {
		out[i] = c.r
	}
	return out
}

// trimResults keeps reranked results while each scores at least trimRatio of the one before and of
// the best, and always the best.
func trimResults(rs []KBResult) []KBResult {
	best := *rs[0].RerankScore
	if best <= 0 {
		return rs[:1]
	}
	n := 1
	for ; n < len(rs); n++ {
		cur, prev := *rs[n].RerankScore, *rs[n-1].RerankScore
		if cur < trimRatio*prev || cur < trimRatio*best {
			break
		}
	}
	return rs[:n]
}
