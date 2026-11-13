// Package cloudsync moves one of this Meko's datapacks to the linked Meko Cloud, all or nothing: a
// precheck against the account's plan, then the datapack's export is imported by Meko Cloud under
// the same id, and only once Meko Cloud says it's done is the datapack deleted here. From then on
// its id isn't this Meko's, so /mcp and /api/v1 send every call on it to Meko Cloud
// (docs/cloud.md, "Syncing a datapack to Meko Cloud").
package cloudsync

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/memory"
)

// Meko Cloud's import routes, after its API URL.
const (
	pathPrecheck = "/datapacks/import/precheck"
	pathImport   = "/datapacks/import"
)

// Import states Meko Cloud reports.
const (
	cloudWaiting   = "waiting_upload"
	cloudImporting = "importing"
	cloudDone      = "done"
	cloudFailed    = "failed"
)

// Run states, as GET /datapacks/:id/cloud-sync gives them.
const (
	StateSyncing = "syncing"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Errors a sync can't start with.
var (
	// ErrUnsupported is a linked Meko Cloud without the import routes.
	ErrUnsupported = errors.New("this Meko Cloud can't take synced datapacks yet")
	// ErrNotLinked is a sync while Meko Cloud isn't linked.
	ErrNotLinked = errors.New("link Meko Cloud first: Settings › Meko Cloud, or meko cloud-link")
)

// CloudError is Meko Cloud's refusal of an import call, with its status, code and message.
type CloudError struct {
	Status        int
	Code, Message string
}

func (e *CloudError) Error() string { return "Meko Cloud: " + e.Message }

// Cloud is the linked Meko Cloud's REST API (cloudlink.Service).
type Cloud interface {
	IsLinked() (bool, error)
	Pass(ctx context.Context, method, pathQuery string, body io.Reader, contentType string) (cloudlink.Reply, error)
}

// Check is one of the plan's limits a datapack was checked against: what's used, what the datapack
// adds, and the limit (-1 for none), with Meko Cloud's message when it doesn't fit.
type Check struct {
	Check   string `json:"check"` // datapacks, memory_adds, kb_storage, kb_file_size or searchable_turns
	Used    int64  `json:"used"`
	Adding  int64  `json:"adding"`
	Limit   int64  `json:"limit"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// Precheck is whether a datapack fits the account's plan, with each check.
type Precheck struct {
	OK     bool    `json:"ok"`
	Plan   string  `json:"plan"`
	Checks []Check `json:"checks"`
}

// Run is a datapack's sync as it stands.
type Run struct {
	DatapackID   string    `json:"datapack_id"`
	DatapackName string    `json:"datapack_name"`
	State        string    `json:"state"` // syncing, done or failed
	Step         string    `json:"step"`  // while syncing: exporting, uploading or importing
	Error        string    `json:"error,omitempty"`
	Precheck     *Precheck `json:"precheck,omitempty"` // a failed one
	WebURL       string    `json:"web_url,omitempty"`  // the datapack in Meko Cloud's web app, once done
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitzero"`
	// Phases is the sync's timeline, in order: exporting, uploading, importing (waiting for Meko
	// Cloud to take it), Meko Cloud's own steps as it reports them, then finishing (deleting it here).
	// The last one is open while it runs; a failed run failed in it.
	Phases []Phase `json:"phases"`
	// Upload is the export's bytes sent so far, once uploading starts.
	Upload Progress `json:"upload,omitzero"`
}

// Phase is one step of a sync's timeline.
type Phase struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
}

// Progress is how much of something is done.
type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// Syncer runs syncs, one per datapack at a time.
type Syncer struct {
	Service *memory.Service
	Cloud   Cloud
	// Version is this Meko's, for the export's header.
	Version string
	// TempDir holds the export while it uploads; "" is the system's.
	TempDir string
	Log     *slog.Logger
	// Poll is how often Meko Cloud is asked how the import is going; Timeout, how long a sync waits
	// for it; Retry, how long a failed cancel waits before trying again. Zero is 2 seconds, 30
	// minutes and 2 seconds.
	Poll, Timeout, Retry time.Duration
	// Upload posts the export to Meko Cloud's upload link; nil is an HTTP multipart POST.
	Upload func(ctx context.Context, link string, fields map[string]string, file io.Reader) error

	mu   sync.Mutex
	runs map[string]*Run
}

// Precheck measures one of this Meko's datapacks and asks Meko Cloud whether it fits the plan.
func (s *Syncer) Precheck(ctx context.Context, id string) (Precheck, error) {
	if err := s.needLinked(); err != nil {
		return Precheck{}, err
	}
	id = memory.CanonicalID(id)
	size, err := s.Service.SyncSize(ctx, id)
	if err != nil {
		return Precheck{}, err
	}
	var out Precheck
	err = s.call(ctx, http.MethodPost, pathPrecheck, map[string]any{"datapack_id": id,
		"memories": size.Memories, "searchable_turns": size.SearchableTurns, "files": size.Files,
		"file_bytes": size.FileBytes, "largest_file_bytes": size.LargestFile}, &out)
	return out, err
}

// Start checks a datapack against the plan and, when it fits, syncs it in the background with ctx,
// which should outlive the request. A datapack that doesn't fit gets a failed Run with its precheck,
// and nothing is synced.
func (s *Syncer) Start(ctx context.Context, id string) (Run, error) {
	id = memory.CanonicalID(id)
	d, err := s.Service.Datapack(ctx, id)
	if err != nil {
		return Run{}, err
	}
	pc, err := s.Precheck(ctx, id)
	if err != nil {
		return Run{}, err
	}
	run := &Run{DatapackID: id, DatapackName: d.Name, State: StateSyncing, StartedAt: time.Now().UTC()}
	if !pc.OK {
		run.State, run.Error, run.Precheck, run.EndedAt = StateFailed, "It doesn't fit your Meko Cloud plan.", &pc,
			run.StartedAt
		s.keep(run)
		return *run, nil
	}
	if err = s.Service.BeginSync(id); err != nil {
		return Run{}, err
	}
	s.keep(run)
	s.step(run, "exporting")
	out, _ := s.Status(id) // copied before the sync can change it
	go s.sync(context.WithoutCancel(ctx), run)
	return out, nil
}

// Status is a datapack's latest sync, if it had one since this Meko started.
func (s *Syncer) Status(id string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[memory.CanonicalID(id)]
	if !ok {
		return Run{}, false
	}
	out := *r
	out.Phases = slices.Clone(r.Phases) // the sync goes on changing its own
	return out, true
}

// Resume finishes the syncs a stop interrupted: one Meko Cloud has done deletes its datapack here,
// one still importing is waited for, and one that never got its upload is canceled, leaving its
// datapack here as it was.
func (s *Syncer) Resume(ctx context.Context) {
	jobs, err := s.Service.SyncJobs(ctx)
	if err != nil {
		s.warn("couldn't read the syncs to Meko Cloud that were running", "", err)
		return
	}
	for id, importID := range jobs {
		// HoldSyncs stopped its writes at start already; a sync can't be running for it yet.
		if err = s.Service.BeginSync(id); err != nil && !errors.Is(err, memory.ErrDatapackSyncing) {
			continue
		}
		run := &Run{DatapackID: id, State: StateSyncing, StartedAt: time.Now().UTC()}
		if d, derr := s.Service.Datapack(ctx, id); derr == nil {
			run.DatapackName = d.Name
		}
		s.keep(run)
		s.step(run, "importing")
		go func() {
			st, serr := s.importStatus(ctx, importID)
			if serr == nil && st.Status == cloudWaiting {
				s.fail(ctx, run, importID, shown("The upload stopped when Meko did.", nil))
				return
			}
			s.finish(ctx, run, importID)
		}()
	}
}

func (s *Syncer) sync(ctx context.Context, run *Run) {
	dir, err := os.MkdirTemp(s.TempDir, "meko-sync-")
	if err != nil {
		s.fail(ctx, run, "", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	f, sum, size, err := s.export(ctx, dir, run.DatapackID)
	if err != nil {
		s.fail(ctx, run, "", shown("Exporting it failed.", err))
		return
	}
	defer func() { _ = f.Close() }()

	s.step(run, "uploading")
	var offer struct {
		ImportID string            `json:"import_id"`
		Upload   string            `json:"upload_url"`
		Fields   map[string]string `json:"fields"`
	}
	if err = s.call(ctx, http.MethodPost, pathImport, map[string]any{"datapack_id": run.DatapackID,
		"size_bytes": size, "sha256": sum}, &offer); err != nil {
		s.fail(ctx, run, "", err)
		return
	}
	// Saved before the upload, so a stop from here on is resumed, not forgotten. A stop between the
	// POST above and this leaves Meko Cloud an import waiting for an upload, which it replaces when
	// this datapack's next sync asks for one.
	if err = s.Service.SetSyncJob(ctx, run.DatapackID, offer.ImportID); err != nil {
		s.fail(ctx, run, offer.ImportID, err)
		return
	}
	upload := s.Upload
	if upload == nil {
		upload = postFile
	}
	s.mu.Lock()
	run.Upload = Progress{Total: size}
	s.mu.Unlock()
	if err = upload(ctx, offer.Upload, offer.Fields, &progressReader{r: f, s: s, run: run}); err != nil {
		s.fail(ctx, run, offer.ImportID, shown("Uploading it to Meko Cloud failed.", err))
		return
	}
	if err = s.call(ctx, http.MethodPost, pathImport+"/"+url.PathEscape(offer.ImportID)+"/start", nil,
		nil); err != nil {
		s.fail(ctx, run, offer.ImportID, err)
		return
	}
	s.finish(ctx, run, offer.ImportID)
}

// finish waits for Meko Cloud's import, then deletes the datapack here; a failed import leaves it.
func (s *Syncer) finish(ctx context.Context, run *Run, importID string) {
	s.step(run, "importing")
	poll, timeout := s.Poll, s.Timeout
	if poll == 0 {
		poll = 2 * time.Second
	}
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.importStatus(ctx, importID)
		var refused *CloudError
		switch {
		case errors.As(err, &refused) && refused.Status/100 == 4:
			// Meko Cloud doesn't have this import (any more): waiting won't change that.
			s.fail(ctx, run, importID, err)
			return
		case err != nil && time.Now().After(deadline):
			s.fail(ctx, run, importID, err)
			return
		case err != nil:
			// Meko Cloud unreachable for a moment: the import goes on there, so ask again.
		case st.Status == cloudDone:
			s.phase(run, "finishing")
			if err = s.Service.FinishSync(ctx, run.DatapackID); err != nil {
				// Meko Cloud has it, and this Meko still serves its copy: keep the job and the hold on
				// writes (a write here would be lost), so the next start tries the delete again.
				s.end(run, StateFailed, "Meko Cloud has it, but deleting it on this computer failed. "+
					"Restart Meko to finish the sync.", err)
				return
			}
			_ = s.Service.SetSyncJob(ctx, run.DatapackID, "")
			s.mu.Lock()
			run.State, run.Step, run.WebURL, run.EndedAt = StateDone, "", st.WebURL, time.Now().UTC()
			closePhase(run)
			s.mu.Unlock()
			return
		case st.Status == cloudFailed:
			msg := st.Error
			if msg == "" {
				msg = "Meko Cloud couldn't import it"
			}
			s.fail(ctx, run, "", shown(msg, nil)) // Meko Cloud removed what it made
			_ = s.Service.SetSyncJob(ctx, run.DatapackID, "")
			return
		case time.Now().After(deadline):
			s.fail(ctx, run, importID, shown("The import took too long in Meko Cloud.", nil))
			return
		case st.Step != "":
			s.phase(run, st.Step) // Meko Cloud's step: reading, creating, memories_and_conversations, ...
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

type importState struct {
	Status string `json:"status"`
	Step   string `json:"step"`
	Error  string `json:"error"`
	WebURL string `json:"web_url"`
}

func (s *Syncer) importStatus(ctx context.Context, importID string) (importState, error) {
	var st importState
	err := s.call(ctx, http.MethodGet, pathImport+"/"+url.PathEscape(importID), nil, &st)
	return st, err
}

// fail ends a run that didn't make it: Meko Cloud's import, if there is one, is canceled (Meko
// Cloud removes what it made), and the datapack takes writes again, unchanged. When the cancel itself
// can't get through, the job and the hold on writes stay, since Cloud's import could still finish:
// the next start cancels it, or finishes the sync if Cloud did.
func (s *Syncer) fail(ctx context.Context, run *Run, importID string, err error) {
	if importID != "" {
		if cerr := s.cancel(ctx, importID); cerr != nil {
			s.warn("couldn't cancel a datapack's import in Meko Cloud; it's held until Meko restarts",
				run.DatapackID, cerr)
			s.end(run, StateFailed, publicMessage(err)+" Meko couldn't cancel the import in Meko Cloud, so "+
				"the datapack takes no writes until Meko restarts and settles it.", err)
			return
		}
		if serr := s.Service.SetSyncJob(ctx, run.DatapackID, ""); serr != nil {
			s.warn("couldn't forget a canceled import", run.DatapackID, serr)
		}
	}
	s.Service.EndSync(run.DatapackID)
	s.end(run, StateFailed, publicMessage(err), err)
}

// cancel cancels an import in Meko Cloud, a few times over a short while; one Meko Cloud doesn't
// have is canceled already.
func (s *Syncer) cancel(ctx context.Context, importID string) error {
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cmp.Or(s.Retry, 2*time.Second)):
			}
		}
		err = s.call(ctx, http.MethodDelete, pathImport+"/"+url.PathEscape(importID), nil, nil)
		var refused *CloudError
		if err == nil || errors.As(err, &refused) && refused.Status == http.StatusNotFound {
			return nil
		}
	}
	return err
}

// end records how a run ended. msg is what the CLI and the web UI show; err, with its detail,
// goes to the log only.
func (s *Syncer) end(r *Run, state, msg string, err error) {
	s.mu.Lock()
	r.State, r.Step, r.Error, r.EndedAt = state, "", msg, time.Now().UTC()
	closePhase(r)
	s.mu.Unlock()
	if err != nil {
		s.warn("a datapack's sync to Meko Cloud failed", r.DatapackID, err)
	}
}

// shownError is a failure with a message for people; its cause goes to the log.
type shownError struct {
	msg   string
	cause error
}

func (e *shownError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + " " + e.cause.Error()
}

func (e *shownError) Unwrap() error { return e.cause }

func shown(msg string, cause error) error { return &shownError{msg: msg, cause: cause} }

// publicMessage is what a failed sync shows: Meko Cloud's own message, a message written for people,
// or, for anything that went wrong on this computer, a generic line (the detail is in the log).
func publicMessage(err error) string {
	var sh *shownError
	var refused *CloudError
	switch {
	case errors.As(err, &sh):
		return sh.msg
	case errors.As(err, &refused) && refused.Message != "":
		return "Meko Cloud: " + refused.Message
	case errors.Is(err, ErrUnsupported), errors.Is(err, ErrNotLinked):
		return err.Error()
	}
	return "Something went wrong on this computer while syncing it; Meko's log has the details."
}

func (s *Syncer) export(ctx context.Context, dir, id string) (*os.File, string, int64, error) {
	f, err := os.Create(filepath.Join(dir, "datapack.zip"))
	if err != nil {
		return nil, "", 0, err
	}
	h := sha256.New()
	if err = s.Service.ExportDatapack(ctx, id, s.Version, func() io.Writer { return io.MultiWriter(f, h) }); err != nil {
		_ = f.Close()
		return nil, "", 0, err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, "", 0, err
	}
	return f, hex.EncodeToString(h.Sum(nil)), size, nil
}

// call is a JSON call to Meko Cloud's REST API, its data decoded into out (nil to drop it).
func (s *Syncer) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r, ct = bytes.NewReader(b), "application/json"
	}
	rep, err := s.Cloud.Pass(ctx, method, path, r, ct)
	if err != nil {
		return err
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	jerr := json.Unmarshal(rep.Body, &env)
	switch {
	case rep.Status == http.StatusNotFound && (jerr != nil || env.Error == nil || env.Error.Code != "NOT_FOUND" ||
		path == pathPrecheck):
		return ErrUnsupported
	// A Meko Cloud from before the import routes sends the precheck to its /datapacks/:datapack_id
	// routes, whose check refuses the id "import" with this 400.
	case path == pathPrecheck && rep.Status == http.StatusBadRequest && jerr == nil && env.Error != nil &&
		env.Error.Code == "VALIDATION_ERROR" && env.Error.Message == "invalid datapack_id format":
		return ErrUnsupported
	case jerr == nil && env.Error != nil:
		return &CloudError{Status: rep.Status, Code: env.Error.Code, Message: env.Error.Message}
	case rep.Status/100 != 2 || jerr != nil:
		return fmt.Errorf("an answer from Meko Cloud that isn't its API's: %d", rep.Status)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func (s *Syncer) needLinked() error {
	linked, err := s.Cloud.IsLinked()
	if err != nil {
		return err
	}
	if !linked {
		return ErrNotLinked
	}
	return nil
}

func (s *Syncer) keep(r *Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string]*Run{}
	}
	s.runs[r.DatapackID] = r
}

// step sets the run's step, which also starts that phase.
func (s *Syncer) step(r *Run, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Step = step
	startPhase(r, step)
}

// phase starts the named phase of the run's timeline, unless it's the one under way.
func (s *Syncer) phase(r *Run, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	startPhase(r, name)
}

func startPhase(r *Run, name string) {
	if n := len(r.Phases); n > 0 && r.Phases[n-1].Name == name && r.Phases[n-1].EndedAt.IsZero() {
		return
	}
	closePhase(r)
	r.Phases = append(r.Phases, Phase{Name: name, StartedAt: time.Now().UTC()})
}

func closePhase(r *Run) {
	if n := len(r.Phases); n > 0 && r.Phases[n-1].EndedAt.IsZero() {
		r.Phases[n-1].EndedAt = time.Now().UTC()
	}
}

// progressReader counts the export's bytes as the upload reads them, for the run's Upload.
type progressReader struct {
	r   io.Reader
	s   *Syncer
	run *Run
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.s.mu.Lock()
		p.run.Upload.Done += int64(n)
		p.s.mu.Unlock()
	}
	return n, err
}

func (s *Syncer) warn(msg, id string, err error) {
	if s.Log != nil {
		s.Log.Warn(msg, "datapack_id", id, "error", err)
	}
}

// postFile posts the export to a presigned upload link: the fields first, then the file as "file",
// as Meko Cloud's knowledge-base uploads take it.
func postFile(ctx context.Context, link string, fields map[string]string, file io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	// Built before the writer starts: a request that can't be made leaves nothing blocked on the pipe.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, link, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	go func() {
		werr := func() error {
			for k, v := range fields {
				if ferr := mw.WriteField(k, v); ferr != nil {
					return ferr
				}
			}
			part, perr := mw.CreateFormFile("file", "datapack.zip")
			if perr != nil {
				return perr
			}
			if _, perr = io.Copy(part, file); perr != nil {
				return perr
			}
			return mw.Close()
		}()
		_ = pw.CloseWithError(werr)
	}()
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G107: the link Meko Cloud's import gave
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("the upload link answered %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
