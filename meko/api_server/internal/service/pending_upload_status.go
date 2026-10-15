package service

import (
	"fmt"
	"path"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// Values of DocumentStatusInfo.State. They span both halves of an upload's
// life — waiting for bytes, then indexing them — because a caller polls one
// key across the whole of it and should not have to know where the boundary
// is.
const (
	// docStateAwaitingUpload: promised, and either the bytes have not been
	// seen in the bucket yet, or they have but dist_rag has not yet discovered
	// the new source they belong to (AWAITING_DISCOVERY). Covers the watcher
	// actively looking either way, since a caller can do nothing differently
	// for either.
	docStateAwaitingUpload = "awaiting_upload"
	// docStateAbandoned: the authorization expired with nothing stored, or what
	// was stored was not the type it claimed. Terminal; retrying means asking
	// for a new URL.
	docStateAbandoned = "abandoned"
	// docStateOversized: the object arrived over the cap its URL was signed
	// under and was deleted. Terminal.
	docStateOversized = "oversized"
	// docStateIndexing: registered in dist_rag and somewhere in the pipeline.
	docStateIndexing  = "indexing"
	docStateCompleted = "completed"
	docStateFailed    = "failed"
)

// DocumentStatusInfo is one file's resolved progress, from whichever half of
// its lifecycle answered. Exactly one of Document/Upload is set.
type DocumentStatusInfo struct {
	Document *repository.DocumentProgress
	Upload   *repository.PendingUpload
	// Filename is the name the file was uploaded under, not the key it is
	// stored at — that one carries ids and a content hash.
	Filename string
	State    string
	// Message is for showing a person: an agent relaying a refusal has
	// something to say beyond the state word.
	Message string
}

// DocumentStatus reports one file's progress across both halves of a
// presigned upload's life: registered in dist_rag, or still tracked only by
// a promise row. found is false when neither knows this key (the caller's
// 404 case); a lookup that fails is an error, never a "not found", since a
// polling client reads 404 as "this upload is gone".
//
// dist_rag is checked first because it outranks the promise row: a terminal
// row (ABANDONED) can outlive a document the client registered itself.
//
// It is checked again when no row is found. The two reads aren't one snapshot
// (the tables can live on different databases), and the watcher registers a
// document and then deletes its row in between them; read in that order, a
// poll could miss both and answer 404 for an upload that just succeeded. The
// row is only deleted once the document exists, so after a missing row the
// second read finds it.
func (s *PendingUploadService) DocumentStatus(
	dp *models.Datapack, documentKey string,
) (info DocumentStatusInfo, found bool, err error) {
	if info, found, err = s.registeredStatus(dp, documentKey); err != nil || found {
		return info, found, err
	}

	pending, err := s.Lookup(dp.ID, documentKey)
	if err != nil {
		return DocumentStatusInfo{}, false, err
	}
	if pending == nil {
		return s.registeredStatus(dp, documentKey)
	}
	state, message := pendingUploadState(*pending)
	return DocumentStatusInfo{
		Filename: pending.Filename,
		State:    state,
		Message:  message,
		Upload:   pending,
	}, true, nil
}

// registeredStatus is DocumentStatus's answer from dist_rag, with found false
// when the document isn't registered.
func (s *PendingUploadService) registeredStatus(
	dp *models.Datapack, documentKey string,
) (DocumentStatusInfo, bool, error) {
	detail, ok, err := s.documentDetail(dp, documentKey)
	if err != nil || !ok {
		return DocumentStatusInfo{}, false, err
	}
	state, message := documentIndexingState(detail.Status)
	return DocumentStatusInfo{
		Filename: path.Base(documentKey),
		State:    state,
		Message:  message,
		Document: &detail,
	}, true, nil
}

// documentDetail returns the dist_rag progress for a single document key
// within a datapack, if it is registered.
func (s *PendingUploadService) documentDetail(
	dp *models.Datapack, documentKey string,
) (repository.DocumentProgress, bool, error) {
	if s.ragSvc == nil {
		return repository.DocumentProgress{}, false, nil
	}
	sourceURIs, err := s.kbSvc.GetSourceURIs(dp)
	if err != nil {
		return repository.DocumentProgress{}, false, fmt.Errorf("source uris for %s: %w", dp.ID, err)
	}
	if len(sourceURIs) == 0 {
		return repository.DocumentProgress{}, false, nil
	}
	detail, err := s.ragSvc.GetDocumentStatus(sourceURIs, documentKey)
	if err != nil {
		return repository.DocumentProgress{}, false, fmt.Errorf("document status for %s: %w", documentKey, err)
	}
	if detail == nil {
		return repository.DocumentProgress{}, false, nil
	}
	return *detail, true, nil
}

// pendingUploadState maps a promise row onto a state and a sentence. The
// terminal states prefer the recorded reason: it is the specific one ("not a
// PDF", "over the 5 MB cap"), and this is the only place a caller ever sees
// it.
func pendingUploadState(p repository.PendingUpload) (state, message string) {
	switch p.State {
	case repository.PendingUploadAbandoned:
		state, message = docStateAbandoned,
			"The upload did not complete before its authorization expired."
	case repository.PendingUploadOversized:
		state, message = docStateOversized,
			"The uploaded file was larger than the limit it was authorized for."
	case repository.PendingUploadAwaitingDiscovery:
		return docStateAwaitingUpload,
			"The file has arrived and is waiting to be picked up for indexing."
	default:
		return docStateAwaitingUpload, "Waiting for the uploaded file to arrive in storage."
	}
	if p.LastError != "" {
		message = p.LastError
	}
	return state, message
}

// documentIndexingState maps a dist_rag document status onto a state.
// Everything that is not one of the two terminal values is in flight: an
// allow-list of QUEUED/PROCESSING would silently report NOT_STARTED and RETRY
// as finished.
func documentIndexingState(documentStatus string) (state, message string) {
	switch documentStatus {
	case "COMPLETED":
		return docStateCompleted, "The file is indexed and searchable."
	case "FAILED":
		return docStateFailed, "Indexing failed for this file."
	default:
		return docStateIndexing, "The file is registered and being indexed."
	}
}
