package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

const (
	// UploadGracePeriod is the extra time the watcher keeps looking after the
	// upload URL expires. The URL limits when the upload may start, not when it
	// must finish, so a big file that started just before expiry can still be
	// arriving; giving up at UploadURLTTL would abandon an upload that is about
	// to succeed.
	UploadGracePeriod = 10 * time.Minute

	// watcherAwakeInterval is the gap between passes while there is work. A
	// browser upload finishes in seconds, so this is what makes the watcher a
	// participant in the normal path rather than a nightly cleanup: an agent
	// that never calls upload-complete still sees its file indexed promptly.
	watcherAwakeInterval = 1 * time.Second
	// watcherIdleInterval is the gap when the last pass found nothing. A
	// presign nudges the watcher awake, so this is only the backstop that
	// recovers rows another pod promised, or ones whose lease expired.
	watcherIdleInterval = 60 * time.Second

	// watcherBatch caps rows per pass, since each one costs an S3 HeadObject
	// and every claimed row holds a lease no other pod can take.
	watcherBatch = 20
	// watcherLease must outlast a pass over a full batch, including the ranged
	// GET and the dist_rag write. Too short and a second pod starts working a
	// row this one still holds; too long and a crashed pod's rows wait that
	// long to be recovered.
	watcherLease = 2 * time.Minute
	// watcherLeaseMargin is the lease a row must have left to be started: more
	// than one row's worst case, the 30s source resolution (sourceResolveTimeout)
	// plus its S3 calls. A row started with less could outlive the lease, be
	// reclaimed by another pod, and be verified or registered by both.
	watcherLeaseMargin = 45 * time.Second

	// watcherBackoffCap bounds the per-row backoff (see
	// repository.backoff). It is the worst case for noticing an upload
	// that took a while to land, so it trades that latency against the S3 calls
	// spent asking: at 15s a promise costs ~60 looks over its whole 15-minute
	// window instead of ~900.
	watcherBackoffCap = 15 * time.Second

	// watcherStuckAfter is how far past its deadline a live row must be to be
	// reported as stuck (see PendingUploadHealth.StuckRows). A lease covers the
	// pass that abandons a row as its deadline passes.
	watcherStuckAfter = watcherLease

	// watcherRetryWindow is how long past a row's ExpiresAt the watcher keeps
	// retrying a failure that is ours rather than the upload's: S3 or the
	// database erroring, or dist_rag still discovering the source. ExpiresAt
	// alone is minutes after the presign, too short to ride out an outage;
	// with no bound at all, a row that keeps failing would be polled, and
	// hold its declared size against the owner's quota, forever.
	watcherRetryWindow = 24 * time.Hour

	// watcherPurgeInterval is how often an idle watcher deletes old ABANDONED
	// and OVERSIZED rows, terminalRowRetention how long one is kept first: long
	// enough that a caller asking why an upload failed is still told.
	watcherPurgeInterval = time.Hour
	terminalRowRetention = 7 * 24 * time.Hour
	// watcherPurgeBatch and watcherPurgeMaxBatches bound one purge, so a
	// backlog is worked off over several intervals rather than in one burst.
	watcherPurgeBatch      = 500
	watcherPurgeMaxBatches = 10

	// datapackDeletedReason is what a row whose datapack was deleted is
	// abandoned with.
	datapackDeletedReason = "the datapack was deleted before the upload was added to its knowledge base"
)

// documentStatusNotStarted is a dist_rag document no pipeline run was ever
// queued for, documentStatusFailed one whose last run failed.
const (
	documentStatusNotStarted = "NOT_STARTED"
	documentStatusFailed     = "FAILED"
)

// PendingUploadService tracks objects api_server has authorized but not yet
// seen, and reconciles them into dist_rag.
//
// It exists because a presigned upload takes api_server out of the transfer:
// the bytes go client -> S3, and the only thing that reports them landing is
// the client calling back. An agent has no obligation to — it can upload and
// stop there, leaving an object stored and unindexed with nothing to notice. So
// the address is written down at presign time and a watcher goes looking for it,
// which makes registration depend on nothing the client does after the upload.
//
// Every presign is recorded, the browser's included. The row is also what the
// storage quota counts an upload by until it is registered, and the client
// says who it is, so exempting the browser would let any caller opt out of the
// quota by claiming to be one. meko_ui still registers its own uploads with
// upload-complete and create; the watcher then finds the document and retires
// the row.
type PendingUploadService struct {
	repo        *repository.PendingUploadRepo
	s3Svc       *S3Service
	ragSvc      *RagService
	kbSvc       *KnowledgeBaseService
	srcResolver *SourceResolver

	// lastPurge is when this pod's watcher last purged terminal rows. Only the
	// watcher goroutine reads or writes it.
	lastPurge time.Time

	// nudge wakes the watcher when a promise is written, so the first look
	// happens about a second after the URL is issued instead of up to
	// watcherIdleInterval later. Buffered and written non-blocking: a lost
	// nudge only costs latency, since the idle tick finds the row anyway.
	nudge chan struct{}
}

func NewPendingUploadService(
	repo *repository.PendingUploadRepo,
	s3Svc *S3Service,
	ragSvc *RagService,
	kbSvc *KnowledgeBaseService,
	srcResolver *SourceResolver,
) *PendingUploadService {
	return &PendingUploadService{
		repo:        repo,
		s3Svc:       s3Svc,
		ragSvc:      ragSvc,
		kbSvc:       kbSvc,
		srcResolver: srcResolver,
		nudge:       make(chan struct{}, 1),
	}
}

// Promise records an upload api_server has just authorized, and wakes the
// watcher.
func (s *PendingUploadService) Promise(p repository.PendingUpload) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("pending uploads are not configured")
	}
	if p.ExpiresAt.IsZero() {
		p.ExpiresAt = time.Now().Add(UploadURLTTL + UploadGracePeriod)
	}
	if err := s.repo.Promise(p); err != nil {
		return err
	}
	select {
	case s.nudge <- struct{}{}:
	default:
	}
	return nil
}

// PromiseUploadRequest carries what the caller knows right after issuing a
// presign, for PromiseUpload to turn into a promise row.
type PromiseUploadRequest struct {
	S3Key        string
	DatapackID   string
	OwnerUserID  string
	UploaderID   string
	Filename     string
	DeclaredSize int64
	// PerFileCap is the requester's cap today. Ignored when AlreadyUploaded —
	// see PromiseUpload's doc comment.
	PerFileCap      int64
	AlreadyUploaded bool
}

// PromiseUpload records a just-issued presign as a promise, deciding the cap
// the watcher's oversized backstop (see resolve) checks the object against.
//
// On the AlreadyUploaded (dedup) path no policy was signed for this call —
// the object already exists, and its bytes necessarily already cleared
// whatever cap applied when THEY were uploaded. PerFileCap here is only
// today's caller's cap, which can be lower than the original uploader's
// (e.g. a Free-tier collaborator requesting the same content a Pro uploader
// already stored). Promise's ON CONFLICT revives the existing row by s3_key,
// so recording PerFileCap here would overwrite a correct, higher
// MaxSizeBytes with a wrong, lower one — and the oversized backstop would
// then delete a file that was legitimately accepted under the real cap. 0
// disables that backstop for a row nothing here actually authorized.
func (s *PendingUploadService) PromiseUpload(req PromiseUploadRequest) error {
	maxSizeBytes := req.PerFileCap
	if req.AlreadyUploaded {
		maxSizeBytes = 0
	}
	return s.Promise(repository.PendingUpload{
		S3Key:        req.S3Key,
		DatapackID:   req.DatapackID,
		OwnerUserID:  req.OwnerUserID,
		UploaderID:   req.UploaderID,
		Filename:     req.Filename,
		DeclaredSize: req.DeclaredSize,
		MaxSizeBytes: maxSizeBytes,
	})
}

// PresignRequest is one upload-url request, already validated by the handler.
type PresignRequest struct {
	Datapack   *models.Datapack
	UploaderID string
	// AccountType is the requester's tier, empty when unknown. It decides
	// whether a storage quota applies.
	AccountType string
	Filename    string
	ContentHash string
	Size        int64
	PerFileCap  int64
}

// KBQuotaExceededError means the owner's storage quota, registered plus
// in-flight bytes, has no room for this upload.
type KBQuotaExceededError struct {
	Response TierLimitResponse
}

func (e *KBQuotaExceededError) Error() string { return e.Response.Message }

// PresignUpload checks the storage quota, signs the upload and records its
// promise row.
//
// For a tier with a storage cap the three run under the owner's row lock, so
// concurrent presigns for one owner take turns: each sees the bytes the earlier
// ones promised, and none passes against a total the others are about to
// raise. In-flight uploads count at their declared size (see SumPendingSizes).
//
// Content already stored in the datapack sends no bytes, so it is answered
// AlreadyUploaded before the quota is looked at: a user at their limit can
// still re-request a file they have.
//
// A failed promise write fails the presign. The row is the only thing that
// registers an upload whose client never calls back, and what the quota counts
// until it is registered, so a URL issued without one would leave the file
// unindexed and uncounted.
func (s *PendingUploadService) PresignUpload(ctx context.Context, req PresignRequest) (*PresignedUpload, error) {
	if s == nil || s.repo == nil || s.s3Svc == nil {
		return nil, fmt.Errorf("pending uploads are not configured")
	}
	dp := req.Datapack
	var result *PresignedUpload
	presignAndPromise := func() error {
		up, err := s.s3Svc.ExistingUpload(ctx, dp.UserID, dp.ID, req.ContentHash)
		if err != nil {
			return fmt.Errorf("check existing upload for %s/%s: %w", dp.UserID, req.Filename, err)
		}
		var signedCap int64
		if up == nil {
			key := UploadKey(dp.UserID, dp.ID, req.ContentHash, req.Filename)
			if signedCap, err = s.signedCap(req, key); err != nil {
				return err
			}
			if up, err = s.s3Svc.SignUploadURL(ctx, key, req.ContentHash, req.Size, signedCap); err != nil {
				return fmt.Errorf("generate upload url for %s/%s: %w", dp.UserID, req.Filename, err)
			}
		}
		if err := s.PromiseUpload(PromiseUploadRequest{
			S3Key:           up.Key,
			DatapackID:      dp.ID,
			OwnerUserID:     dp.UserID,
			UploaderID:      req.UploaderID,
			Filename:        req.Filename,
			DeclaredSize:    req.Size,
			PerFileCap:      signedCap,
			AlreadyUploaded: up.AlreadyUploaded,
		}); err != nil {
			return fmt.Errorf("record pending upload %s: %w", up.Key, err)
		}
		result = up
		return nil
	}
	if !IsGatedUserType(req.AccountType) {
		if err := presignAndPromise(); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err := s.kbSvc.WithUserRowLock(ctx, dp.UserID, presignAndPromise); err != nil {
		return nil, err
	}
	return result, nil
}

// signedCap is the most this upload may declare: the per-file cap, lowered to
// the owner's remaining storage quota on a gated tier. It returns
// KBQuotaExceededError when there is no room for the file.
//
// The file being presigned counts once, at what its row will hold after this
// presign: a re-presign updates the row for key rather than adding one, and
// keeps the larger declared size while an earlier URL may still be used.
func (s *PendingUploadService) signedCap(req PresignRequest, key string) (int64, error) {
	if !IsGatedUserType(req.AccountType) {
		return req.PerFileCap, nil
	}
	owner := req.Datapack.UserID
	registered, err := s.kbSvc.SumDocumentSizesByOwner(owner)
	if err != nil {
		return 0, fmt.Errorf("sum registered sizes for %s: %w", owner, err)
	}
	others, thisKey, err := s.repo.SumPendingSizes(owner, key)
	if err != nil {
		return 0, fmt.Errorf("sum pending sizes for %s: %w", owner, err)
	}
	limit := LimitsFor(req.AccountType).MaxKBStorageBytes
	current := registered + others
	remaining := limit - current
	need := max(req.Size, thisKey)
	if remaining <= 0 || need > remaining {
		slog.Info("kb upload blocked by tier cap",
			"user_id", owner,
			"user_type", req.AccountType,
			"current_bytes", current,
			"incoming_bytes", need,
			"cap", limit,
		)
		return 0, &KBQuotaExceededError{Response: KBStorageLimitResponse(req.AccountType)}
	}
	return min(req.PerFileCap, remaining), nil
}

// Lookup returns the promise recorded for one key in one datapack, or nil if
// none is on record.
//
// Nil is the answer for a *successful* upload too, since the row is retired
// once the document is registered in dist_rag, so a caller reporting on one
// file must check dist_rag.documents before reading a nil row as "nothing was
// ever promised".
func (s *PendingUploadService) Lookup(datapackID, s3Key string) (*repository.PendingUpload, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	return s.repo.GetByKey(datapackID, s3Key)
}

// Cancel marks a promised upload as abandoned with a caller-supplied reason,
// for a caller that has rejected or deleted the object itself, so
// document-status reports that reason instead of waiting out the row's
// deadline. It overrides a watcher's lease (see PendingUploadRepo.Abandon), and
// is a no-op when no live row is on record for this key: the row is
// bookkeeping for the watcher, not the source of truth for whether the upload
// was rejected.
//
// Also a no-op while the row's URL may still be used. A retry through it would
// land the object again, and a row abandoned by then would leave that upload
// unindexed and uncounted; left live, the watcher registers it or abandons the
// row at its deadline.
func (s *PendingUploadService) Cancel(datapackID, s3Key, reason string) {
	if s == nil || s.repo == nil {
		return
	}
	if _, err := s.repo.Abandon(datapackID, s3Key, reason, UploadGracePeriod); err != nil {
		slog.Warn("pending upload: cancel failed",
			"datapack_id", datapackID, "s3_key", s3Key, "error", err)
	}
}

// RunWatcher reconciles promised uploads until ctx is canceled, closing done
// on exit.
//
// The loop runs hot (watcherAwakeInterval) while a pass finds work. When one
// comes back empty it asks how long until the next row is due and sleeps that
// long, falling back to watcherIdleInterval when there is nothing live at all.
// That middle case is what makes the per-row backoff real: rows can be live but
// not yet due, and sleeping the full idle interval through that would undo the
// backoff's own schedule. Promise's nudge covers the first look.
func (s *PendingUploadService) RunWatcher(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	if s == nil || s.repo == nil || s.s3Svc == nil || !s.s3Svc.Enabled() {
		slog.Warn("pending upload watcher: not configured, exiting")
		return
	}

	slog.Info("pending upload watcher started",
		"awake_interval", watcherAwakeInterval,
		"idle_interval", watcherIdleInterval,
		"batch", watcherBatch,
		"lease", watcherLease,
	)

	// Starts hot rather than idle: a pod restart can find rows already
	// waiting (leftover from before it went down, or ones whose lease just
	// expired), and starting idle would sit on that backlog for up to
	// watcherIdleInterval before ever looking.
	timer := time.NewTimer(watcherAwakeInterval)
	defer timer.Stop()

	// From here the watcher just sits waiting for one of three things, and
	// reacts to whichever happens first:
	//   1. the server is shutting down            -> stop looping
	//   2. a fresh promise just came in (a nudge)  -> go look right now
	//   3. the timer we were sleeping on went off  -> take a pass, then go
	//      back to sleep for however long the pass says to: a short nap
	//      (watcherAwakeInterval) if it found work, a longer one (idleFor)
	//      if the queue came back empty
	for {
		select {
		case <-ctx.Done():
			slog.Info("pending upload watcher stopped")
			return

		case <-s.nudge:
			wakeNow(timer)

		case <-timer.C:
			s.onTimerFired(ctx, timer)
		}
	}
}

// wakeNow makes timer fire immediately instead of waiting out whatever
// interval it was already counting down, which is how a fresh promise gets
// looked at right away instead of sitting until the next scheduled tick.
func wakeNow(timer *time.Timer) {
	if !timer.Stop() {
		// The timer had already gone off moments ago and left a tick sitting
		// in the channel unread. Drain it first, or the Reset below would be
		// satisfied instantly by that stale tick instead of starting a fresh
		// wait.
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(0)
}

// onTimerFired takes one pass over the queue and reschedules timer based on
// what it found -- a short nap if tick reports there's more work, a longer
// one if the queue came back empty. Named to read alongside wakeNow: each
// case in RunWatcher's select calls one clearly-named handler.
func (s *PendingUploadService) onTimerFired(ctx context.Context, timer *time.Timer) {
	timer.Reset(s.tick(ctx))
}

// tick takes one pass over the queue and reports how long to wait before the
// next one: right back to the hot interval if that pass found work, or
// however long idleFor recommends when it came back empty.
func (s *PendingUploadService) tick(ctx context.Context) time.Duration {
	if s.pass(ctx) == 0 {
		s.purgeIfDue()
		return s.idleFor()
	}
	return watcherAwakeInterval
}

// purgeIfDue deletes terminal rows past terminalRowRetention, at most once per
// watcherPurgeInterval. Run from idle passes only, so it never delays a row.
func (s *PendingUploadService) purgeIfDue() {
	if time.Since(s.lastPurge) < watcherPurgeInterval {
		return
	}
	s.lastPurge = time.Now()
	var total int64
	for range watcherPurgeMaxBatches {
		n, err := s.repo.PurgeTerminal(terminalRowRetention, watcherPurgeBatch)
		if err != nil {
			slog.Warn("pending upload watcher: purging terminal rows failed", "error", err)
			break
		}
		total += n
		if n < watcherPurgeBatch {
			break
		}
	}
	if total > 0 {
		slog.Info("pending upload watcher: purged terminal rows", "rows", total)
	}
}

// pass claims one batch and resolves each row, returning how many rows it
// looked at. Zero means the queue is drained, which is what puts the watcher
// back to sleep.
func (s *PendingUploadService) pass(ctx context.Context) int {
	claimed, err := s.repo.Claim(watcherBatch, watcherLease, watcherBackoffCap)
	if err != nil {
		slog.Error("pending upload watcher: claim failed", "error", err)
		return 0
	}
	if len(claimed) == 0 {
		return 0
	}

	// owed holds, per source, the rows whose document is registered but not
	// yet queued. They are retired only once the queue call succeeds.
	owed := map[string][]repository.PendingUpload{}
	// Sequential: each resolve does at least one S3 HeadObject (more on the
	// paths that reach dist_rag), and checking ctx between rows is what lets
	// a shutdown mid-batch stop after the row in flight instead of the whole
	// batch.
	for i, p := range claimed {
		if time.Until(p.LeaseExpiresAt) < watcherLeaseMargin {
			// The rows share one lease. Hand the rest back rather than start
			// one that could outlive it; the next pass claims them afresh.
			slog.Warn("pending upload watcher: lease running out; releasing the rest of the batch",
				"released", len(claimed)-i)
			for _, rest := range claimed[i:] {
				s.release(rest)
			}
			break
		}
		if ctx.Err() != nil {
			// Shutting down. Leave the rest leased; they are claimable again
			// once the lease expires, and re-registering is idempotent. Break
			// rather than return so the rows already registered still get
			// their queue call below.
			break
		}
		if sourceID := s.resolve(ctx, p); sourceID != "" {
			owed[sourceID] = append(owed[sourceID], p)
		}
	}

	s.queueAndRetire(owed)
	slog.Info("pending upload watcher pass complete",
		"claimed", len(claimed), "sources_queued", len(owed))
	return len(claimed)
}

// queueAndRetire makes one queue call per source, not per document: it queues
// every pending document of the source, so a batch that arrived together needs
// one. A row is retired only after its call succeeds. On failure it goes back
// for another look, which finds its document still NOT_STARTED and owes the
// call again, so a document is never left registered and unqueued.
func (s *PendingUploadService) queueAndRetire(owed map[string][]repository.PendingUpload) {
	for sourceID, rows := range owed {
		if err := s.ragSvc.BuildSource(sourceID); err != nil {
			slog.Error("pending upload watcher: queuing the source's documents failed",
				"source_id", sourceID, "rows", len(rows), "error", err)
			for _, p := range rows {
				s.recheck(p, "queue the source's documents: "+err.Error())
			}
			continue
		}
		for _, p := range rows {
			s.retire(p)
		}
	}
}

// resolve decides one row's fate. A non-empty result is the source whose
// documents must be queued before the row may be retired; the row is left
// held for pass to retire or recheck. "" means resolve finished with the row.
//
// The three outcomes map onto what is actually known: registered or permanently
// wrong rows are finished with (Retire / Fail), and everything else — including
// "the bytes haven't arrived yet", which is the common case — goes back for
// another look. Nothing is failed for a fact that could still change, until
// the row's deadline has passed (see recheck).
func (s *PendingUploadService) resolve(ctx context.Context, p repository.PendingUpload) string {
	dp := &models.Datapack{ID: p.DatapackID, UserID: p.OwnerUserID}

	// p.State here is whatever repo.Claim returned for it — the row's state
	// from *before* that claim, never the IN_PROGRESS the claim just wrote (see
	// Claim's doc comment). This branch is unreachable if that ever regresses.
	if p.State == repository.PendingUploadAwaitingDiscovery {
		return s.resolveAwaitingDiscovery(ctx, p, dp)
	}
	return s.verifyAndRegister(ctx, p, dp, "")
}

// verifyAndRegister checks a landed object against what its URL authorized
// and registers it. settledSourceID is set for a row coming back from
// AWAITING_DISCOVERY: the source resolveAwaitingDiscovery just found settled,
// whose worker may already have created the document.
func (s *PendingUploadService) verifyAndRegister(
	ctx context.Context, p repository.PendingUpload, dp *models.Datapack, settledSourceID string,
) string {
	afterDiscovery := settledSourceID != ""
	docURI := DocumentURI(s.s3Svc.Bucket(), p.S3Key)
	// retry puts the row back for another look in the state it came from. A
	// discovery row must stay one: put back as AWAITING_UPLOAD, its next look
	// would find dist_rag's document and stop there.
	retry := func(lastError string) {
		if afterDiscovery {
			s.recheckAwaitingDiscovery(p)
			return
		}
		s.recheck(p, lastError)
	}

	obj, err := s.s3Svc.HeadObject(ctx, p.S3Key)
	switch {
	case errors.Is(err, ErrObjectNotFound):
		// Not there. Only the deadline can turn this into a "no": before it,
		// the upload may still be in flight or not yet started.
		if time.Now().After(p.ExpiresAt) {
			reason := "upload never arrived before the authorization expired"
			if afterDiscovery {
				reason = "the uploaded file is no longer in storage"
			}
			s.fail(p, repository.PendingUploadAbandoned, reason)
			return ""
		}
		retry("")
		return ""
	case err != nil:
		// S3 wouldn't answer. Says nothing about the upload, so it must not
		// count against it — see ErrObjectNotFound.
		slog.Warn("pending upload watcher: head object failed",
			"s3_key", p.S3Key, "attempts", p.Attempts, "error", err)
		retry(err.Error())
		return ""
	}

	// Already registered — by the client's own upload-complete/create round
	// trip, or by an earlier pass whose Retire or queue call didn't land. The
	// row's purpose is served once the document exists and has been queued; a
	// document still NOT_STARTED was never queued, so it is owed the call. So
	// is a FAILED one: a row for it is a retry (a re-presign of the same
	// content), which create answers by queuing it again, as it does here.
	//
	// Checked before the branches below because they delete the object, and
	// a document row that outlives its object is one users can see but not
	// download. The key is content-addressed, so a re-presign of a file already
	// stored and indexed lands on this row — under today's cap, which may be
	// lower than the one it was authorized under if the tier changed.
	//
	// A document dist_rag's own CREATE_SOURCE worker created lacks the meko
	// details (size, uploader) the storage quota and attribution read, so it
	// goes on to RegisterDocument below to write them, as CreateKB's
	// waitBuildAndPopulateDetails does for the same case. That is every
	// discovery hand-off, and an agent's object a worker listed first.
	if !afterDiscovery {
		doc, exists, regErr := s.kbSvc.DocumentStateForURI(dp, docURI)
		if regErr != nil {
			slog.Warn("pending upload watcher: registration check failed",
				"s3_key", p.S3Key, "error", regErr)
			s.recheck(p, regErr.Error())
			return ""
		}
		if exists && doc.HasMekoDetails {
			if doc.Status == documentStatusNotStarted || doc.Status == documentStatusFailed {
				s.markRegistered(p)
				return doc.SourceID
			}
			s.retire(p)
			return ""
		}
	}

	// The checks below are backstops. The signed policy holds the body to
	// exactly the declared size (itself within the signed cap) and to the
	// checksum of the content hash in the key, so S3 rejects an upload that
	// breaks any of them. They catch an object that reached this key some
	// other way: a bug, another write path, or a backend that doesn't enforce
	// a policy condition the way S3 does.
	//
	// An empty ChecksumSHA256 means the backend didn't return one even when
	// asked: "can't verify", not a mismatch.
	checksumVerified := false
	if obj.ChecksumSHA256 != "" {
		if declaredHex, ok := contentHashFromKey(p.S3Key); ok {
			declaredChecksum, convErr := sha256HexToChecksum(declaredHex)
			if convErr == nil && declaredChecksum != obj.ChecksumSHA256 {
				slog.Warn("pending upload watcher: object checksum does not match its content-addressed key",
					"s3_key", p.S3Key, "declared_hash", declaredHex, "actual_checksum", obj.ChecksumSHA256)
				s.deleteObject(ctx, dp, p.S3Key)
				s.fail(p, repository.PendingUploadAbandoned,
					"the uploaded file's content does not match what was declared when this upload "+
						"was authorized — request a new upload URL and try again")
				return ""
			}
			checksumVerified = convErr == nil
		}
	}

	// The size checks are skipped when nothing was signed (MaxSizeBytes is 0,
	// the AlreadyUploaded path): those bytes were already in the bucket, fixed
	// by the content hash in the key, so a size this request got wrong says
	// nothing about them — and deleting would take an object another upload
	// may be about to register.
	if p.MaxSizeBytes > 0 && obj.Size > p.MaxSizeBytes {
		slog.Warn("pending upload watcher: object exceeds the cap it was signed under",
			"s3_key", p.S3Key, "size", obj.Size, "max_size_bytes", p.MaxSizeBytes)
		s.deleteObject(ctx, dp, p.S3Key)
		s.fail(p, repository.PendingUploadOversized,
			fmt.Sprintf("object is %d bytes, over the %d-byte cap it was authorized under",
				obj.Size, p.MaxSizeBytes))
		return ""
	}
	// A verified checksum proves the content, and a content hash has one size,
	// so a different declared size can only mean the row is stale: a
	// re-presign keeps the larger declared size while an earlier URL may still
	// be used (see PendingUploadRepo.Promise), and the object may have arrived
	// through either URL. Only an unverified object is judged by its size.
	if p.MaxSizeBytes > 0 && !checksumVerified && obj.Size != p.DeclaredSize {
		slog.Warn("pending upload watcher: object size does not match what was declared at presign time",
			"s3_key", p.S3Key, "declared_size", p.DeclaredSize, "actual_size", obj.Size)
		s.deleteObject(ctx, dp, p.S3Key)
		s.fail(p, repository.PendingUploadAbandoned,
			fmt.Sprintf("the uploaded file is %d bytes, but %d bytes were declared when this upload "+
				"was authorized — request a new upload URL with the correct size and try again",
				obj.Size, p.DeclaredSize))
		return ""
	}

	// Not a backstop: S3 never looks at content, so this is the only check
	// that an agent's file is what its name claims.
	contentType, err := VerifyObjectContent(ctx, s.s3Svc, p.S3Key)
	if err != nil {
		var rejected *ContentRejectedError
		if !errors.As(err, &rejected) {
			// Couldn't read the bytes, so the file isn't condemned — a
			// transport error here must not delete good data.
			slog.Warn("pending upload watcher: content verification unavailable",
				"s3_key", p.S3Key, "error", err)
			retry(err.Error())
			return ""
		}
		// The object is positively not what its name claims. Nothing will ever
		// index it, so it is deleted rather than left to be paid for and
		// re-sniffed on every later sweep.
		slog.Warn("pending upload watcher: object rejected",
			"s3_key", p.S3Key, "reason", rejected.Reason)
		s.deleteObject(ctx, dp, p.S3Key)
		s.fail(p, repository.PendingUploadAbandoned, rejected.Reason)
		return ""
	}

	// A discovery row's source was resolved, and found settled, moments ago;
	// resolving it again would take the datapack lock across dist_rag twice.
	sourceID, wait := settledSourceID, false
	var resolveErr error
	if !afterDiscovery {
		sourceID, wait, resolveErr = s.sourceForRegistration(ctx, dp)
	}
	if errors.Is(resolveErr, repository.ErrDatapackNotFound) {
		s.abandonForDeletedDatapack(p)
		return ""
	}
	if resolveErr != nil {
		slog.Error("pending upload watcher: failed to resolve source",
			"datapack_id", p.DatapackID, "s3_key", p.S3Key, "error", resolveErr)
		retry(resolveErr.Error())
		return ""
	}
	if wait {
		// dist_rag's CREATE_SOURCE worker is listing this prefix itself, and
		// inserting alongside it is how duplicate document rows appear. The
		// row is parked, not retired, until the source has finished (see
		// resolveAwaitingDiscovery): retiring now would leave a window where
		// neither this row nor dist_rag knows the key, and a status poll would
		// get a bare 404.
		slog.Info("pending upload watcher: source still being discovered by dist_rag, waiting for it",
			"datapack_id", p.DatapackID, "source_id", sourceID, "s3_key", p.S3Key)
		s.recheckAwaitingDiscovery(p)
		return ""
	}

	if err := RegisterDocument(
		ctx, s.ragSvc, s.s3Svc.Bucket(), sourceID, p.UploaderID, contentType, obj,
	); err != nil {
		slog.Error("pending upload watcher: failed to register document",
			"datapack_id", p.DatapackID, "source_id", sourceID,
			"s3_key", p.S3Key, "error", err)
		retry(err.Error())
		return ""
	}

	slog.Info("pending upload registered",
		"datapack_id", p.DatapackID, "source_id", sourceID,
		"s3_key", p.S3Key, "size", obj.Size,
		"content_type", contentType, "attempts", p.Attempts,
		"after_discovery", afterDiscovery)
	s.markRegistered(p)
	return sourceID
}

// sourceForRegistration resolves the datapack's dist_rag source and reports
// whether registering into it now would race dist_rag's own CREATE_SOURCE
// worker: the source is brand new, or still QUEUED/IN_PROGRESS. That worker
// runs asynchronously, so a source created moments ago by this watcher or by
// CreateKB may still be listing the prefix. CreateKB's
// shouldSkipAPIDocumentSync gates on the same two-part check. A status it
// can't read counts as "still listing": guessing otherwise could insert
// alongside the worker, and waiting is bounded (see recheckAwaitingDiscovery).
func (s *PendingUploadService) sourceForRegistration(
	ctx context.Context, dp *models.Datapack,
) (sourceID string, wait bool, err error) {
	sourceURI := s.kbSvc.BuildDatapackPrefix(dp.UserID, dp.ID)
	sourceID, isNewSource, err := s.srcResolver.Resolve(ctx, dp, sourceURI)
	if err != nil {
		return "", false, err
	}
	if isNewSource {
		return sourceID, true, nil
	}
	status, err := s.ragSvc.GetSourceStatus(sourceID)
	if err != nil {
		slog.Warn("pending upload watcher: failed to read source status; waiting",
			"datapack_id", dp.ID, "source_id", sourceID, "error", err)
		return sourceID, true, nil
	}
	return sourceID, status == SourceStatusQueued || status == SourceStatusInProgress, nil
}

// resolveAwaitingDiscovery re-checks a row handed off to dist_rag's
// CREATE_SOURCE worker. While the source is still being discovered the row
// keeps waiting. Once it isn't, the row is registered like any other:
// RegisterDocument writes the meko details onto the document dist_rag created,
// or inserts the document if dist_rag's listing ran before the object landed
// and missed it.
func (s *PendingUploadService) resolveAwaitingDiscovery(
	ctx context.Context, p repository.PendingUpload, dp *models.Datapack,
) string {
	sourceID, wait, err := s.sourceForRegistration(ctx, dp)
	if errors.Is(err, repository.ErrDatapackNotFound) {
		s.abandonForDeletedDatapack(p)
		return ""
	}
	if err != nil {
		slog.Warn("pending upload watcher: failed to resolve source for a discovery hand-off",
			"datapack_id", p.DatapackID, "s3_key", p.S3Key, "error", err)
		s.recheckAwaitingDiscovery(p)
		return ""
	}
	if wait {
		s.recheckAwaitingDiscovery(p)
		return ""
	}
	slog.Info("pending upload watcher: source discovery finished, registering",
		"datapack_id", p.DatapackID, "source_id", sourceID, "s3_key", p.S3Key)
	return s.verifyAndRegister(ctx, p, dp, sourceID)
}

// The bookkeeping writes below only log on failure: the S3 and dist_rag work
// is already done by the time they run, and a lost write costs one more look
// at a row whose handling is idempotent. Each is fenced on the lease Claim
// returned, so ErrLeaseLost means the row changed hands (another pod, or a
// Cancel) and there is nothing to retry.

// markRegistered drops a row whose document now exists out of the pending
// quota total; the document counts there instead, at its real size. The row
// itself lives on until its indexing is queued (see queueAndRetire).
func (s *PendingUploadService) markRegistered(p repository.PendingUpload) {
	logBookkeeping("mark registered", p, s.repo.MarkRegistered(p.ID, p.LeaseExpiresAt))
}

func (s *PendingUploadService) retire(p repository.PendingUpload) {
	logBookkeeping("retire", p, s.repo.Retire(p.ID, p.LeaseExpiresAt))
}

func (s *PendingUploadService) fail(p repository.PendingUpload, state, reason string) {
	logBookkeeping("fail", p, s.repo.Fail(p.ID, p.LeaseExpiresAt, state, reason))
}

// recheck releases the row for another look. lastError is why this look
// didn't resolve it: empty for "the bytes haven't arrived", whose deadline is
// ExpiresAt and is checked by the caller; set for a failure of ours, which is
// retried until watcherRetryWindow past ExpiresAt and then abandoned.
func (s *PendingUploadService) recheck(p repository.PendingUpload, lastError string) {
	if lastError != "" && time.Now().After(p.ExpiresAt.Add(watcherRetryWindow)) {
		slog.Warn("pending upload watcher: giving up on a row that kept failing",
			"id", p.ID, "s3_key", p.S3Key, "attempts", p.Attempts, "last_error", lastError)
		s.fail(p, repository.PendingUploadAbandoned,
			"the upload could not be registered; request a new upload URL and try again")
		return
	}
	logBookkeeping("recheck", p, s.repo.Recheck(p.ID, p.LeaseExpiresAt, lastError))
}

// recheckAwaitingDiscovery releases a row back to waiting on dist_rag's
// discovery, bounded the same way as a failure of ours (see recheck): a source
// dist_rag never finishes discovering would otherwise park the row forever.
func (s *PendingUploadService) recheckAwaitingDiscovery(p repository.PendingUpload) {
	if time.Now().After(p.ExpiresAt.Add(watcherRetryWindow)) {
		slog.Warn("pending upload watcher: giving up waiting for dist_rag's discovery",
			"id", p.ID, "s3_key", p.S3Key, "attempts", p.Attempts)
		s.fail(p, repository.PendingUploadAbandoned,
			"the upload could not be indexed; request a new upload URL and try again")
		return
	}
	logBookkeeping("recheck (awaiting discovery)", p, s.repo.RecheckAwaitingDiscovery(p.ID, p.LeaseExpiresAt))
}

// release puts an untouched row back in the state it was claimed from.
func (s *PendingUploadService) release(p repository.PendingUpload) {
	if p.State == repository.PendingUploadAwaitingDiscovery {
		logBookkeeping("release", p, s.repo.RecheckAwaitingDiscovery(p.ID, p.LeaseExpiresAt))
		return
	}
	logBookkeeping("release", p, s.repo.Recheck(p.ID, p.LeaseExpiresAt, ""))
}

// abandonForDeletedDatapack ends a row whose datapack no longer exists. Retried
// instead, it would be looked at every few seconds until watcherRetryWindow
// ran out, counting against the owner's quota the whole time.
func (s *PendingUploadService) abandonForDeletedDatapack(p repository.PendingUpload) {
	slog.Info("pending upload watcher: datapack was deleted; abandoning the upload",
		"datapack_id", p.DatapackID, "s3_key", p.S3Key)
	s.fail(p, repository.PendingUploadAbandoned, datapackDeletedReason)
}

func logBookkeeping(op string, p repository.PendingUpload, err error) {
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrLeaseLost):
		slog.Info("pending upload watcher: row changed hands; dropping it",
			"op", op, "id", p.ID, "s3_key", p.S3Key)
	default:
		slog.Warn("pending upload watcher: bookkeeping write failed",
			"op", op, "id", p.ID, "s3_key", p.S3Key, "error", err)
	}
}

// deleteObject goes through S3KeyIsSafeToDelete, the guard every KB-side delete
// shares, rather than trusting resolve's registration check further up: a
// client's upload-complete/create can register the key in between, and deleting
// then leaves a document users can see but not download. It fails closed.
func (s *PendingUploadService) deleteObject(ctx context.Context, dp *models.Datapack, key string) {
	switch safe, err := s.kbSvc.S3KeyIsSafeToDelete(dp, key); {
	case err != nil:
		slog.Error("pending upload watcher: cannot tell whether rejected object is still referenced; keeping it",
			"s3_key", key, "error", err)
		return
	case !safe:
		slog.Warn("pending upload watcher: rejected object is referenced by a document; keeping it",
			"s3_key", key)
		return
	}
	slog.Warn("pending upload watcher: deleting rejected object", "s3_key", key)
	if err := s.s3Svc.DeleteObject(ctx, key); err != nil {
		slog.Error("pending upload watcher: failed to delete rejected object",
			"s3_key", key, "error", err)
	}
}

// idleFor reports how long to sleep after a pass that claimed nothing, and logs
// queue depth on the way past — an empty pass is the one moment those numbers
// are both cheap and meaningful, since an idle watcher and a stalled one look
// identical in the logs otherwise.
//
// An empty pass does not mean an empty queue: rows can be waiting out their
// backoff or another pod's lease, and the sleep is sized to whichever comes due
// first so neither waits longer than it has to.
func (s *PendingUploadService) idleFor() time.Duration {
	h, err := s.repo.Health(watcherStuckAfter, watcherBackoffCap)
	if err != nil {
		slog.Warn("pending upload watcher: health query failed", "error", err)
		return watcherIdleInterval
	}
	if h.Live == 0 {
		return watcherIdleInterval
	}

	level := slog.LevelInfo
	if h.StuckRows > 0 {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "pending upload queue depth",
		"live", h.Live,
		"oldest_age", h.OldestAge.Round(time.Second),
		"stuck", h.StuckRows,
		"next_due_in", h.NextDueIn.Round(time.Millisecond),
	)

	// Never longer than the idle interval, so a NextDueIn built from a lease far
	// in the future can't park the watcher past its own backstop, and never
	// shorter than a pass, so a row due now doesn't spin the loop.
	if h.NextDueIn < watcherAwakeInterval {
		return watcherAwakeInterval
	}
	return min(h.NextDueIn, watcherIdleInterval)
}
