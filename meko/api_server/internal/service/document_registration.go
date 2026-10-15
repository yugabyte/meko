package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// DocumentURI is the address dist_rag stores for an S3 object. Derived from the
// bucket rather than passed in, so every writer names the same object the same
// way — writers find an existing document by (source_id, URI), so two
// spellings of one object would be two documents.
func DocumentURI(bucket, key string) string {
	return fmt.Sprintf("s3://%s/%s", bucket, key)
}

// DatapackKeyPrefix is the bare S3 key prefix for everything belonging to one
// datapack: "<user_id>/<datapack_id>/", with no "s3://<bucket>/" wrapper.
// Shared so every call site scopes keys to the same prefix DocumentURI would
// build the full URI from.
func DatapackKeyPrefix(userID, datapackID string) string {
	return fmt.Sprintf("%s/%s/", userID, datapackID)
}

// DocumentDetailsJSON returns the JSON written to dist_rag.documents.document_details.
//
// Shape:
//
//		{"meko": {"user_id": "<uploader>", "size": <bytes>, "uploaded_at": "<RFC3339>"}}
//
//	  - user_id      authenticated uploader (NOT the datapack owner — multiple
//	    collaborators can upload to the same shared datapack, so each
//	    file has to record who actually sent it).
//	  - size         bytes from S3 Contents.Size; 0 means "unknown" rather
//	    than "empty file" (S3 occasionally omits Size on certain
//	    listing edge cases).
//	  - uploaded_at  S3 LastModified timestamp (RFC3339 UTC) — the moment
//	    the object was written to the bucket. This is the canonical
//	    "uploaded" time shown in the UI. Zero time is omitted so
//	    legacy rows without it degrade gracefully.
//
// Output is wire-ready JSONB. Returns nil only on json.Marshal failure
// — caller logs + falls back to "{}" so the upsert never fails because
// of details encoding.
func DocumentDetailsJSON(uploaderUserID string, size int64, uploadedAt time.Time) []byte {
	uploadInfo := map[string]any{
		"user_id": uploaderUserID,
		"size":    size,
	}
	if !uploadedAt.IsZero() {
		uploadInfo["uploaded_at"] = uploadedAt.UTC().Format(time.RFC3339)
	}
	// Namespaced under "meko" because the column belongs to dist_rag, which
	// is free to write its own keys alongside ours.
	payload := map[string]any{
		"meko": uploadInfo,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Shouldn't happen for this fixed shape; log + fall through so
		// the document is still inserted with empty details rather
		// than the whole upload failing.
		slog.Warn("DocumentDetailsJSON: marshal failed", "error", err)
		return nil
	}
	return b
}

// RegisterDocument upserts one S3 object into dist_rag.documents, with the
// uploader / size / uploaded-at details the UI reads.
//
// Three writers go through here — the CreateKB sweep, the post-build populate,
// and the pending-upload watcher — sharing it because they write the same row.
// InsertDocument's UPDATE-then-INSERT runs under the source row lock, so a
// later writer updates the row an earlier one created instead of adding a
// second one; dist_rag.documents has no unique constraint, so a writer that
// bypassed InsertDocument would insert duplicates.
//
// contentType is a parameter rather than read off obj: callers pass the type
// VerifyObjectContent resolved from the object's bytes, except that
// waitBuildAndPopulateDetails falls back to obj.ContentType -- the type the
// file name's extension implies -- when the bytes can't be read.
func RegisterDocument(
	ctx context.Context, ragSvc *RagService, bucket, sourceID, uploaderUserID, contentType string, obj S3Object,
) error {
	details := DocumentDetailsJSON(uploaderUserID, obj.Size, obj.UploadedAt)
	return ragSvc.InsertDocument(
		ctx, sourceID, obj.Key, DocumentURI(bucket, obj.Key), obj.ETag, contentType, details)
}
