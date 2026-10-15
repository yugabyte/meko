package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"meko/api_server/internal/models"
)

// SourceResolver maps a datapack to the dist_rag source backing its knowledge
// base, creating one when there isn't one yet.
//
// It lives here rather than on a handler because the pending-upload watcher
// registers documents from a background goroutine, which has no echo context —
// and both it and the request paths must resolve sources identically, or a
// document registered by one becomes invisible to the other.
type SourceResolver struct {
	kbSvc  *KnowledgeBaseService
	ragSvc *RagService
}

func NewSourceResolver(kbSvc *KnowledgeBaseService, ragSvc *RagService) *SourceResolver {
	return &SourceResolver{kbSvc: kbSvc, ragSvc: ragSvc}
}

// sourceResolveTimeout bounds Resolve's locked section, which calls dist_rag
// on another database while holding the datapack row lock. Without it a hung
// dist_rag connection would hold the lock, and block every rename, tier change
// and delete of the datapack, for as long as the connection hung.
var sourceResolveTimeout = 30 * time.Second

// Resolve returns the datapack's source id. isNewSource is true only when this
// call created it, which is what lets callers pick WaitAndBuild (new) over
// BuildSource (already discoverable).
//
// dist_rag.sources has no unique constraint on source_uri, so two concurrent
// callers resolving the same brand-new datapack could both see "no source
// yet" and both create one -- there's no dist_rag row to lock at that point,
// since the source doesn't exist. The datapack's own row, guaranteed to
// already exist, stands in as the shared serialization point instead.
//
// A source that already exists, which is every call after a datapack's first
// upload, is returned without the lock: the lock only stops two callers from
// both creating one, and a caller that goes on to create re-checks under it.
func (r *SourceResolver) Resolve(
	ctx context.Context, dp *models.Datapack, sourceURI string,
) (sourceID string, isNewSource bool, err error) {
	if dp == nil {
		return "", false, fmt.Errorf("datapack is required")
	}
	if r == nil || r.ragSvc == nil {
		return "", false, fmt.Errorf("rag service is not configured")
	}

	cachedID, exists, cacheErr := r.cachedSource(ctx, dp)
	if cacheErr != nil {
		return "", false, cacheErr
	}
	if exists {
		return cachedID, false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, sourceResolveTimeout)
	defer cancel()
	err = r.kbSvc.WithDatapackRowLock(ctx, dp.ID, func() error {
		var lockErr error
		sourceID, isNewSource, lockErr = r.resolveLocked(ctx, dp, sourceURI)
		return lockErr
	})
	if err != nil {
		return "", false, err
	}
	return sourceID, isNewSource, nil
}

// cachedSource returns the source id cached for the datapack, and whether
// dist_rag still has that source. A cached id whose source was deleted comes
// back with exists false.
func (r *SourceResolver) cachedSource(
	ctx context.Context, dp *models.Datapack,
) (sourceID string, exists bool, err error) {
	cachedSourceIDs, err := r.kbSvc.GetSourceIDs(dp)
	if err != nil {
		return "", false, fmt.Errorf("check cached sources: %w", err)
	}
	if len(cachedSourceIDs) == 0 {
		return "", false, nil
	}
	sourceID = cachedSourceIDs[0]
	exists, err = r.ragSvc.SourceExists(ctx, sourceID)
	if err != nil {
		return "", false, fmt.Errorf("validate cached source: %w", err)
	}
	return sourceID, exists, nil
}

// resolveLocked is Resolve's body, run while WithDatapackRowLock holds the
// datapack row lock.
func (r *SourceResolver) resolveLocked(
	ctx context.Context, dp *models.Datapack, sourceURI string,
) (sourceID string, isNewSource bool, err error) {
	sourceID, exists, err := r.cachedSource(ctx, dp)
	if err != nil {
		return "", false, err
	}
	if exists {
		return sourceID, false, nil
	}
	if sourceID != "" {
		// A cached mapping whose dist_rag source has since been deleted is
		// stale; drop it and fall through to recreate.
		slog.Warn("stale KB source cache, clearing",
			"source_id", sourceID, "datapack_id", dp.ID)
		if derr := r.kbSvc.DeleteByDatapack(dp); derr != nil {
			slog.Error("failed to clear stale KB source cache",
				"datapack_id", dp.ID, "error", derr)
		}
	}

	// The source may already exist in dist_rag without a cached mapping (e.g.
	// created out of band); adopt it and persist the mapping.
	sourceID, err = r.ragSvc.LookupSourceByURI(ctx, sourceURI)
	if err != nil {
		return "", false, fmt.Errorf("lookup source: %w", err)
	}
	if sourceID != "" {
		if insErr := r.kbSvc.InsertWithID(dp.ID, sourceID, sourceURI, nil); insErr != nil {
			return "", false, fmt.Errorf("save knowledge base: %w", insErr)
		}
		return sourceID, false, nil
	}

	sourceID, err = r.ragSvc.CreateSource(ctx, sourceURI, dp.ID)
	if err != nil {
		return "", false, fmt.Errorf("create source: %w", err)
	}
	if err := r.ragSvc.AddSourceToIndex(ctx, sourceID); err != nil {
		return "", false, fmt.Errorf("add source to index: %w", err)
	}
	if err := r.kbSvc.InsertWithID(dp.ID, sourceID, sourceURI, nil); err != nil {
		return "", false, fmt.Errorf("save knowledge base: %w", err)
	}
	return sourceID, true, nil
}
