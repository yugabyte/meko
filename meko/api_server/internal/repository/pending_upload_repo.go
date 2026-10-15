package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrLeaseLost means a claimant's write matched no row: its lease expired and
// another pod reclaimed the row, or Abandon or Retire got there first. The row
// is someone else's now, so the caller drops it rather than retrying.
var ErrLeaseLost = errors.New("pending upload lease lost")

// Pending upload states. The same five the table's CHECK constraint allows, so
// a typo here is a loud error rather than a row stranded in a state nothing
// matches.
const (
	PendingUploadAwaiting   = "AWAITING_UPLOAD"
	PendingUploadInProgress = "IN_PROGRESS"
	// PendingUploadAwaitingDiscovery is a row waiting on dist_rag's own
	// CREATE_SOURCE worker: its source is new or still QUEUED/IN_PROGRESS, so
	// that worker is listing the prefix and may create the document itself,
	// and writing alongside it is how duplicate documents appear. Claimable on
	// the same backoff as AWAITING_UPLOAD; a claimed row re-checks the source
	// and keeps waiting until it is neither, then registers like any other
	// row (writing the meko details onto dist_rag's document, or inserting it
	// if the listing missed the object). The status gate, not skipping source
	// resolution, is what keeps this from racing the worker.
	PendingUploadAwaitingDiscovery = "AWAITING_DISCOVERY"
	PendingUploadAbandoned         = "ABANDONED"
	PendingUploadOversized         = "OVERSIZED"
)

// PendingUpload is one promised upload: an object api_server has authorized and
// expects to appear in the bucket.
type PendingUpload struct {
	CreatedAt time.Time
	ExpiresAt time.Time
	// LeaseExpiresAt is the lease Claim stamped on the row, set only on rows
	// Claim returns. It doubles as the claim's fencing token: every write the
	// claimant makes passes it back, so a pod whose lease has lapsed can't
	// overwrite a row another pod now holds.
	LeaseExpiresAt time.Time
	ID             string
	S3Key          string
	DatapackID     string
	OwnerUserID    string
	UploaderID     string
	Filename       string
	State          string
	// LastError is why the last look didn't resolve the row. On a terminal
	// state it is the reason the upload was refused, which is what a caller
	// asking after their file is told.
	LastError string
	// DeclaredSize is the size the client declared at presign, and what the
	// quota counts the upload at until its document is registered. The signed
	// policy holds the body to exactly the size its URL was signed for, but a
	// re-presign keeps the larger of two declared sizes while the earlier URL
	// may still be used (see Promise), so this can exceed what lands.
	DeclaredSize int64
	// MaxSizeBytes is the cap this URL was signed under, not the uploader's cap
	// now.
	MaxSizeBytes int64
	Attempts     int
}

// PendingUploadRepo is meko_system.pending_uploads. Watcher-facing: the claim
// is one statement, so concurrent pods take disjoint batches, and every write a
// claimant makes afterwards is fenced on its lease, so a pod that outlives its
// lease can't overwrite the row's next owner.
type PendingUploadRepo struct {
	systemDB *sql.DB
}

func NewPendingUploadRepo(systemDB *sql.DB) *PendingUploadRepo {
	return &PendingUploadRepo{systemDB: systemDB}
}

// Promise records an authorized upload, or revives the row of one being
// retried.
//
// A retry re-derives the same content-addressed key, so a conflict means "this
// upload is being attempted again", not "duplicate". The revival resets
// attempts, because the old count belongs to a finished attempt and carrying it
// over would let a future attempts cap punish the retry for the original
// failure. created_at restarts with it, so a revived row neither reads as a
// stale one in Health nor jumps the claim queue.
//
// A terminal row always revives to AWAITING_UPLOAD, even one that was
// abandoned out of AWAITING_DISCOVERY: it only expired because dist_rag's
// discovery never produced its document, and that discovery is over, so a
// retry has to go through resolution again. resolve() still won't register
// while the source is QUEUED or IN_PROGRESS.
//
// A live row — IN_PROGRESS on a pod, or AWAITING_DISCOVERY on dist_rag — keeps
// its state, lease, attempts and cap, so a presign can't reset work in
// progress. Its deadline still moves out to the new URL's: a re-presign only
// reaches here with a freshly signed URL when the object hasn't landed, and a
// row left on its old deadline would be abandoned before an upload under the
// new URL could arrive. (A pass that read the old deadline just as it passed
// can still abandon the row; that needs the old URL to have expired unused at
// the same moment as the retry.)
//
// While the existing row's deadline hasn't passed, a URL signed for it may
// still be used, so declared_size and max_size_bytes each keep the larger of
// the old and new values. The quota then never counts less than an upload
// still possible under either URL, and the oversized backstop never deletes
// an object a still-valid URL allowed. The two values can only differ when one
// request got the size wrong: the policy pins the size and the checksum, so
// only the URL signed for the content's true size can land it.
func (r *PendingUploadRepo) Promise(p PendingUpload) error {
	const live = `meko_system.pending_uploads.state IN ('` +
		PendingUploadInProgress + `', '` + PendingUploadAwaitingDiscovery + `')`
	// revive sets col to value on a revived row and leaves a live row's alone.
	revive := func(col, value string) string {
		return col + ` = CASE WHEN ` + live + ` THEN meko_system.pending_uploads.` + col +
			` ELSE ` + value + ` END`
	}
	const urlMayBeLive = `meko_system.pending_uploads.expires_at > NOW()`
	//nolint:gosec // G202: only column names, state constants and EXCLUDED references are concatenated; values are bound.
	if _, err := r.systemDB.Exec(
		`INSERT INTO meko_system.pending_uploads
		     (s3_key, datapack_id, owner_user_id, uploader_id, filename,
		      declared_size, max_size_bytes, expires_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8)
		 ON CONFLICT (s3_key) DO UPDATE SET
		     `+revive("state", `'`+PendingUploadAwaiting+`'`)+`,
		     `+revive("prior_state", "NULL")+`,
		     `+revive("created_at", "NOW()")+`,
		     `+revive("attempts", "0")+`,
		     `+revive("last_error", "NULL")+`,
		     `+revive("uploader_id", "EXCLUDED.uploader_id")+`,
		     `+revive("filename", "EXCLUDED.filename")+`,
		     declared_size = CASE WHEN `+urlMayBeLive+`
		                          THEN GREATEST(meko_system.pending_uploads.declared_size, EXCLUDED.declared_size)
		                          ELSE EXCLUDED.declared_size END,
		     max_size_bytes = CASE WHEN `+urlMayBeLive+`
		                           THEN GREATEST(meko_system.pending_uploads.max_size_bytes, EXCLUDED.max_size_bytes)
		                           ELSE EXCLUDED.max_size_bytes END,
		     `+revive("lease_expires_at", "NULL")+`,
		     `+revive("updated_at", "NOW()")+`,
		     expires_at = CASE WHEN `+live+`
		                       THEN GREATEST(meko_system.pending_uploads.expires_at, EXCLUDED.expires_at)
		                       ELSE EXCLUDED.expires_at END`,
		p.S3Key, p.DatapackID, p.OwnerUserID, p.UploaderID, p.Filename,
		p.DeclaredSize, p.MaxSizeBytes, p.ExpiresAt,
	); err != nil {
		return fmt.Errorf("promise pending upload %s: %w", p.S3Key, err)
	}
	return nil
}

// backoff is how long a row waits before it is worth another look, as an
// interval since its last one: 0 for a row never looked at, then 1, 2, 4, 8 …
// seconds, capped at the value bound to capParam (microseconds, see micros).
// The shift is clamped so a long-lived row can't overflow it.
//
// The delay exists because looking costs an S3 HeadObject and the watcher runs a
// pass per second while any row is live. Without it, a promise nobody fulfills is
// headed ~900 times before it expires, and `attempts` measures elapsed time
// rather than anything diagnostic. With it the same promise costs a few dozen
// looks — the first still happens within a second of the promise (attempts is
// 0, so the CASE below returns 0), but a miss after that is retried on the
// row's own growing schedule, not every second.
//
// Takes the placeholder rather than fixing one, so each query keeps its own
// parameter numbering.
func backoff(capParam string) string {
	return `(CASE WHEN attempts = 0 THEN 0
	              ELSE LEAST(` + capParam + `::BIGINT, (1::BIGINT << LEAST(attempts - 1, 20)) * 1000000)
	         END) * INTERVAL '1 microsecond'`
}

// micros is how durations are bound into SQL: whole microseconds, multiplied
// by INTERVAL '1 microsecond' in the query. Microseconds are the interval
// type's own resolution, so nothing is lost the way rounding to seconds turned
// a sub-second lease into 0.
func micros(d time.Duration) int64 {
	return d.Microseconds()
}

// Claim takes up to limit rows for this pod and stamps a lease on them.
//
// One statement, so there is no read-then-write race and no leader election —
// SKIP LOCKED hands concurrent pods disjoint batches. It commits immediately:
// the S3 work that follows happens outside any transaction, because holding one
// open for seconds against a distributed database is not acceptable. Ownership
// therefore rests on the lease, and a pod that dies mid-pass is recovered when
// the lease expires rather than when a lock releases.
//
// A row is only taken once it is due (see backoff). Lease recovery
// ignores that: the pod holding the row is presumed gone, so its work is due
// now. If it is only slow, its later writes are fenced out (ErrLeaseLost).
//
// The returned State is prior_state, not IN_PROGRESS — the column set on the
// transition into IN_PROGRESS, from whatever the row was before, and left
// alone on every claim after that. That's what makes it durable across a
// crash: a pod that dies mid-pass leaves the row IN_PROGRESS with prior_state
// already recorded, so the pod that recovers it via the expired-lease branch
// still sees AWAITING_DISCOVERY rather than the IN_PROGRESS a plain
// `UPDATE ... RETURNING state` (or a CTE reading state just ahead of this same
// UPDATE) would hand back. resolve() depends on seeing AWAITING_DISCOVERY here
// to keep waiting on dist_rag's discovery instead of going straight to
// registration.
func (r *PendingUploadRepo) Claim(
	limit int, lease, backoffCap time.Duration,
) ([]PendingUpload, error) {
	//nolint:gosec // G202: only state constants and backoff's fixed SQL are concatenated; values are bound.
	rows, err := r.systemDB.Query(
		`UPDATE meko_system.pending_uploads
		    SET state            = '`+PendingUploadInProgress+`',
		        prior_state      = CASE WHEN state = '`+PendingUploadInProgress+`'
		                                THEN prior_state ELSE state END,
		        attempts         = attempts + 1,
		        lease_expires_at = NOW() + $2::BIGINT * INTERVAL '1 microsecond',
		        updated_at       = NOW()
		  WHERE id IN (
		      SELECT id FROM meko_system.pending_uploads
		       WHERE (state IN ('`+PendingUploadAwaiting+`', '`+PendingUploadAwaitingDiscovery+`')
		              AND updated_at + `+backoff("$3")+` <= NOW())
		          OR (state = '`+PendingUploadInProgress+`' AND lease_expires_at < NOW())
		       ORDER BY created_at
		       LIMIT $1
		       FOR UPDATE SKIP LOCKED
		  )
		 RETURNING id, s3_key, datapack_id, owner_user_id, uploader_id, filename,
		           declared_size, max_size_bytes, prior_state, attempts, created_at, expires_at,
		           lease_expires_at`,
		limit, micros(lease), micros(backoffCap),
	)
	if err != nil {
		return nil, fmt.Errorf("claim pending uploads: %w", err)
	}
	defer rows.Close()

	var out []PendingUpload
	for rows.Next() {
		var (
			p          PendingUpload
			priorState sql.NullString
		)
		if err := rows.Scan(
			&p.ID, &p.S3Key, &p.DatapackID, &p.OwnerUserID, &p.UploaderID, &p.Filename,
			&p.DeclaredSize, &p.MaxSizeBytes, &priorState, &p.Attempts, &p.CreatedAt, &p.ExpiresAt,
			&p.LeaseExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan claimed pending upload: %w", err)
		}
		// The CHECK constraint allows a NULL prior_state (see the migration),
		// and a NULL here is possible in principle (a row's very first-ever
		// claim writes it, but nothing guarantees every row reaches this code
		// having gone through that write). A non-nullable string scan target
		// crashing on it would abort the *whole* Query result -- including
		// every already-scanned row in this batch, whose UPDATE already
		// committed regardless -- so this must degrade rather than error.
		// AWAITING_UPLOAD is the safe assumption: resolve() re-runs source
		// resolution and content verification for it, the same as it would
		// for a row that was never claimed before.
		if priorState.Valid {
			p.State = priorState.String
		} else {
			slog.Warn("pending upload watcher: claimed row had no prior_state; assuming default state",
				"assumed_state", PendingUploadAwaiting, "id", p.ID, "s3_key", p.S3Key)
			p.State = PendingUploadAwaiting
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetByKey returns the promise recorded for one key in one datapack, or nil if
// there is none.
//
// Scoped by datapack as well as key so a caller can only be told about uploads
// belonging to the datapack they were authorized for.
//
// Absent is not the same as failed: a registered upload's row is deleted (see
// Retire), so nothing can be concluded from a nil result without checking
// dist_rag.documents first.
func (r *PendingUploadRepo) GetByKey(datapackID, s3Key string) (*PendingUpload, error) {
	var (
		p         PendingUpload
		lastError sql.NullString
	)
	err := r.systemDB.QueryRow(
		`SELECT id, s3_key, datapack_id, owner_user_id, uploader_id, filename,
		        declared_size, max_size_bytes, state, attempts, last_error,
		        created_at, expires_at
		   FROM meko_system.pending_uploads
		  WHERE s3_key = $1 AND datapack_id = $2::uuid`,
		s3Key, datapackID,
	).Scan(
		&p.ID, &p.S3Key, &p.DatapackID, &p.OwnerUserID, &p.UploaderID, &p.Filename,
		&p.DeclaredSize, &p.MaxSizeBytes, &p.State, &p.Attempts, &lastError,
		&p.CreatedAt, &p.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending upload %s: %w", s3Key, err)
	}
	p.LastError = lastError.String
	return &p, nil
}

// SumPendingSizes totals declared_size across the owner's live promises whose
// document isn't registered yet — the bytes a storage-quota check can't see in
// dist_rag.documents. A registered row's document already counts there, at its
// real size.
//
// The row for s3Key, the key being presigned, is left out of others: a
// re-presign updates that row rather than adding one, so counting it as well
// would charge the same file twice. thisKey is instead what that row will
// still hold after the re-presign: its declared_size while its URL may still
// be used (Promise keeps the larger of old and new), else 0.
func (r *PendingUploadRepo) SumPendingSizes(ownerUserID, s3Key string) (others, thisKey int64, err error) {
	if err := r.systemDB.QueryRow(
		`SELECT COALESCE(SUM(declared_size) FILTER (WHERE s3_key <> $2), 0),
		        COALESCE(MAX(declared_size) FILTER (WHERE s3_key = $2 AND expires_at > NOW()), 0)
		   FROM meko_system.pending_uploads
		  WHERE owner_user_id = $1::uuid
		    AND state IN ('`+PendingUploadAwaiting+`', '`+PendingUploadInProgress+`', '`+PendingUploadAwaitingDiscovery+`')
		    AND registered_at IS NULL`,
		ownerUserID, s3Key,
	).Scan(&others, &thisKey); err != nil {
		return 0, 0, fmt.Errorf("sum pending sizes for %s: %w", ownerUserID, err)
	}
	return others, thisKey, nil
}

// Retire deletes a row whose upload is now registered in dist_rag.documents,
// by this service or by the client's own upload-complete/create. Success
// deliberately leaves no row: the record it held has moved.
//
// This and the three writes below are the claimant's: lease is the
// LeaseExpiresAt Claim returned, and a row no longer held under it is left
// alone (ErrLeaseLost).
func (r *PendingUploadRepo) Retire(id string, lease time.Time) error {
	res, err := r.systemDB.Exec(
		`DELETE FROM meko_system.pending_uploads
		  WHERE id = $1::uuid AND `+heldUnder+`$2`,
		id, lease,
	)
	return leaseWriteResult("retire", id, res, err)
}

// heldUnder is the fence every claimant write ends its WHERE clause with; the
// caller appends the placeholder that carries the lease.
const heldUnder = `state = '` + PendingUploadInProgress + `' AND lease_expires_at = `

// leaseWriteResult turns a fenced write's outcome into an error, reporting a
// write that matched nothing as ErrLeaseLost.
func leaseWriteResult(op, id string, res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("%s pending upload %s: %w", op, id, err)
	}
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		return fmt.Errorf("%s pending upload %s: %w", op, id, ErrLeaseLost)
	}
	return nil
}

// MarkRegistered records that the row's document is now registered, so the row
// stops counting against the quota while it waits for its indexing to be
// queued (see SumPendingSizes).
func (r *PendingUploadRepo) MarkRegistered(id string, lease time.Time) error {
	res, err := r.systemDB.Exec(
		`UPDATE meko_system.pending_uploads
		    SET registered_at = NOW(), updated_at = NOW()
		  WHERE id = $1::uuid AND `+heldUnder+`$2`,
		id, lease,
	)
	return leaseWriteResult("mark registered", id, res, err)
}

// Fail marks a row terminal: the watcher never claims it again, and only a
// fresh Promise for the same key (a retry) brings it back. Only for facts that
// can never change: the URL expired, or the object arrived over its cap.
func (r *PendingUploadRepo) Fail(id string, lease time.Time, state, lastError string) error {
	if state != PendingUploadAbandoned && state != PendingUploadOversized {
		return fmt.Errorf("fail pending upload %s: %q is not a terminal state", id, state)
	}
	res, err := r.systemDB.Exec(
		`UPDATE meko_system.pending_uploads
		    SET state = $2, last_error = $3, lease_expires_at = NULL, updated_at = NOW()
		  WHERE id = $1::uuid AND `+heldUnder+`$4`,
		id, state, lastError, lease,
	)
	return leaseWriteResult("fail", id, res, err)
}

// Abandon marks a live row ABANDONED on behalf of a caller that is not its
// claimant — a handler that just rejected and deleted the object itself. It
// is not fenced: it overrides a watcher's lease, whose own write then finds
// the row gone from under it (ErrLeaseLost). Terminal rows keep the reason
// they already have. Reports whether a row changed.
//
// A row whose URL may still be used is left live: the URL can land the object
// again, and only a live row gets that upload registered and counted against
// the quota. urlGrace is how far the row's deadline runs past its URL's
// expiry; the watcher abandons the row itself once the deadline passes.
func (r *PendingUploadRepo) Abandon(
	datapackID, s3Key, lastError string, urlGrace time.Duration,
) (bool, error) {
	res, err := r.systemDB.Exec(
		`UPDATE meko_system.pending_uploads
		    SET state = '`+PendingUploadAbandoned+`', last_error = $3,
		        lease_expires_at = NULL, updated_at = NOW()
		  WHERE s3_key = $1 AND datapack_id = $2::uuid
		    AND state IN ('`+PendingUploadAwaiting+`', '`+PendingUploadInProgress+`', '`+PendingUploadAwaitingDiscovery+`')
		    AND expires_at - $4::BIGINT * INTERVAL '1 microsecond' <= NOW()`,
		s3Key, datapackID, lastError, micros(urlGrace),
	)
	if err != nil {
		return false, fmt.Errorf("abandon pending upload %s: %w", s3Key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("abandon pending upload %s: %w", s3Key, err)
	}
	return n > 0, nil
}

// PurgeTerminal deletes up to limit ABANDONED or OVERSIZED rows last written
// more than olderThan ago, and reports how many it deleted. A terminal row is
// kept only so a caller asking after a failed upload can be told why; once
// nobody will ask, it is just weight on every scan of the table.
//
// The outer state check is evaluated against the row as it is when deleted,
// so a row a retry's Promise revived in the meantime is kept.
func (r *PendingUploadRepo) PurgeTerminal(olderThan time.Duration, limit int) (int64, error) {
	res, err := r.systemDB.Exec(
		`DELETE FROM meko_system.pending_uploads
		  WHERE id IN (
		      SELECT id FROM meko_system.pending_uploads
		       WHERE state IN ('`+PendingUploadAbandoned+`', '`+PendingUploadOversized+`')
		         AND updated_at < NOW() - $1::BIGINT * INTERVAL '1 microsecond'
		       ORDER BY updated_at
		       LIMIT $2
		  )
		    AND state IN ('`+PendingUploadAbandoned+`', '`+PendingUploadOversized+`')`,
		micros(olderThan), limit,
	)
	if err != nil {
		return 0, fmt.Errorf("purge terminal pending uploads: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge terminal pending uploads: %w", err)
	}
	return n, nil
}

// Recheck releases a row for the next pass: "don't know yet, ask again".
//
// This is what most passes do, and it is why a slow upload is looked at again
// on the row's backoff (at most backoffCap later) instead of waiting out its
// whole lease. Everything that is not a
// permanent fact lands here, including AccessDenied — an IAM regression is
// almost always ours, and failing on it would discard every upload for the
// broken window.
func (r *PendingUploadRepo) Recheck(id string, lease time.Time, lastError string) error {
	res, err := r.systemDB.Exec(
		`UPDATE meko_system.pending_uploads
		    SET state = '`+PendingUploadAwaiting+`',
		        last_error = NULLIF($2, ''),
		        lease_expires_at = NULL,
		        updated_at = NOW()
		  WHERE id = $1::uuid AND `+heldUnder+`$3`,
		id, lastError, lease,
	)
	return leaseWriteResult("recheck", id, res, err)
}

// RecheckAwaitingDiscovery releases a row back to AWAITING_DISCOVERY: dist_rag
// has not yet created the document for it, so it is asked again later rather
// than retired.
//
// A separate method from Recheck, not a state argument to it, because
// Recheck's "ask again" always means "the same thing that was being checked
// is still unknown" — this is checking something else (has dist_rag created
// the document yet) and must not be confused with the AWAITING_UPLOAD case a
// plain Recheck would produce.
func (r *PendingUploadRepo) RecheckAwaitingDiscovery(id string, lease time.Time) error {
	res, err := r.systemDB.Exec(
		`UPDATE meko_system.pending_uploads
		    SET state = '`+PendingUploadAwaitingDiscovery+`',
		        lease_expires_at = NULL,
		        updated_at = NOW()
		  WHERE id = $1::uuid AND `+heldUnder+`$2`,
		id, lease,
	)
	return leaseWriteResult("recheck (awaiting discovery)", id, res, err)
}

// PendingUploadHealth is what an operator needs to tell a healthy idle watcher
// from one that is silently doing nothing.
type PendingUploadHealth struct {
	// Live is the number of rows still awaiting upload, in progress, or
	// awaiting dist_rag's own discovery of a new source.
	Live int
	// OldestAge is the age of the oldest live row. A row older than the promise
	// window means the watcher is not draining.
	OldestAge time.Duration
	// StuckRows is the number of live rows past their deadline. By then an
	// upload has either been registered or can never arrive, so a row still
	// live is one the watcher keeps failing on (a broken bucket policy, an
	// unreachable dist_rag) or one whose dist_rag discovery never finished.
	// Measured by deadline, not by attempts: a row waiting out a long dist_rag
	// build, or reclaimed after a pod crash, is looked at often and is fine.
	StuckRows int
	// NextDueIn is how long until the earliest live row can be claimed: its
	// backoff elapsing, or another pod's lease on it expiring. Zero when
	// something is claimable now, and meaningless when Live is 0.
	//
	// The watcher sleeps on this. Without it the backoff would be defeated by
	// the loop's own cadence — a pass that claims nothing would drop to the
	// idle interval and leave a row that comes due in two seconds waiting a
	// minute.
	NextDueIn time.Duration
}

// Health reports the watcher's queue depth and when it next has work.
// stuckAfter is how far past its deadline a live row must be to count as
// stuck; it covers the pass that would abandon a row as its deadline passes.
// backoffCap must match the value passed to Claim, or the two disagree about
// when a row is due.
func (r *PendingUploadRepo) Health(
	stuckAfter, backoffCap time.Duration,
) (PendingUploadHealth, error) {
	var (
		h         PendingUploadHealth
		oldest    sql.NullTime
		nextDue   sql.NullTime
		stuckRows int
	)
	if err := r.systemDB.QueryRow(
		`SELECT COUNT(*),
		        MIN(created_at),
		        COUNT(*) FILTER (WHERE expires_at + $1::BIGINT * INTERVAL '1 microsecond' < NOW()),
		        MIN(CASE WHEN state IN ('`+PendingUploadAwaiting+`', '`+PendingUploadAwaitingDiscovery+`')
		                 THEN updated_at + `+backoff("$2")+`
		                 ELSE lease_expires_at
		            END)
		   FROM meko_system.pending_uploads
		  WHERE state IN ('`+PendingUploadAwaiting+`', '`+PendingUploadInProgress+`', '`+PendingUploadAwaitingDiscovery+`')`,
		micros(stuckAfter), micros(backoffCap),
	).Scan(&h.Live, &oldest, &stuckRows, &nextDue); err != nil {
		return PendingUploadHealth{}, fmt.Errorf("pending upload health: %w", err)
	}
	h.StuckRows = stuckRows
	if oldest.Valid {
		h.OldestAge = time.Since(oldest.Time)
	}
	if nextDue.Valid {
		if d := time.Until(nextDue.Time); d > 0 {
			h.NextDueIn = d
		}
	}
	return h, nil
}
