package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// ---------------------------------------------------------------------------
// Stub repo
// ---------------------------------------------------------------------------

type stubArtifactRepo struct {
	rows map[string]*models.Artifact
}

func newStubRepo() *stubArtifactRepo {
	return &stubArtifactRepo{rows: map[string]*models.Artifact{}}
}

func (r *stubArtifactRepo) FindByHash(_ context.Context, datapackID, hash string) (*models.Artifact, error) {
	return r.rows[datapackID+":"+hash], nil
}

func (r *stubArtifactRepo) Insert(_ context.Context, a *models.Artifact) (*models.Artifact, error) {
	r.rows[a.DatapackID+":"+a.ContentHash] = a
	return a, nil
}

func (r *stubArtifactRepo) TouchLastAccessed(_ context.Context, _, _ string) error { return nil }

func (r *stubArtifactRepo) Reactivate(_ context.Context, datapackID, hash string) (*models.Artifact, error) {
	key := datapackID + ":" + hash
	a := r.rows[key]
	if a == nil {
		return nil, nil
	}
	a.ExpiredAt = nil
	now := time.Now()
	a.LastAccessedAt = &now
	r.rows[key] = a
	return a, nil
}

// ---------------------------------------------------------------------------
// Stub S3
// ---------------------------------------------------------------------------

type stubS3 struct {
	uploaded map[string][]byte
}

func newStubS3() *stubS3 { return &stubS3{uploaded: map[string][]byte{}} }

func (s *stubS3) Enabled() bool { return true }

func (s *stubS3) UploadObject(_ context.Context, userID, datapackID, hash, filename, _ string, data []byte) (string, error) {
	key := userID + "/" + datapackID + "/" + hash + "/" + filename
	s.uploaded[key] = data
	return key, nil
}

func (s *stubS3) DownloadObject(_ context.Context, key string) ([]byte, error) {
	return s.uploaded[key], nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newSvc(repo *stubArtifactRepo, s3 *stubS3) *service.ArtifactService {
	return service.NewArtifactServiceWithIface(repo, s3)
}

// ---------------------------------------------------------------------------
// Tests — Put
// ---------------------------------------------------------------------------

func TestArtifactService_Put_SmallFile_StoredInDB(t *testing.T) {
	repo := newStubRepo()
	s3 := newStubS3()

	data := []byte("hello artifact")
	a, err := newSvc(repo, s3).Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u1", DatapackID: "dp1", Filename: "hello.txt",
		ContentType: "text/plain", Data: data,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if a.StoredIn() != "db" {
		t.Errorf("want stored_in=db, got %s", a.StoredIn())
	}
	if a.ContentHash != sha256hex(data) {
		t.Errorf("hash mismatch: want %s, got %s", sha256hex(data), a.ContentHash)
	}
	if a.S3Key != "" {
		t.Errorf("small file must have empty S3Key, got %s", a.S3Key)
	}
	if len(s3.uploaded) != 0 {
		t.Error("no S3 upload expected for small file")
	}
}

func TestArtifactService_Put_LargeFile_StoredInS3(t *testing.T) {
	repo := newStubRepo()
	s3 := newStubS3()

	data := []byte(strings.Repeat("x", models.MaxInlineBytes+1))
	a, err := newSvc(repo, s3).Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u2", DatapackID: "dp2", Filename: "big.csv",
		ContentType: "text/csv", Data: data,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if a.StoredIn() != "s3" {
		t.Errorf("want stored_in=s3, got %s", a.StoredIn())
	}
	if a.S3Key == "" {
		t.Error("large file must have non-empty S3Key")
	}
	if len(a.InlineContent) != 0 {
		t.Error("large file must have empty InlineContent")
	}
	if len(s3.uploaded) != 1 {
		t.Errorf("expected 1 S3 upload, got %d", len(s3.uploaded))
	}
}

func TestArtifactService_Put_Idempotent(t *testing.T) {
	repo := newStubRepo()
	svc := newSvc(repo, newStubS3())

	req := service.ArtifactPutRequest{
		UserID: "u3", DatapackID: "dp3", Filename: "f.txt",
		ContentType: "text/plain", Data: []byte("same bytes"),
	}
	first, err := svc.Put(context.Background(), req)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := svc.Put(context.Background(), req)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("idempotency broken: IDs differ %s vs %s", first.ID, second.ID)
	}
	if len(repo.rows) != 1 {
		t.Errorf("expected 1 row after 2 identical puts, got %d", len(repo.rows))
	}
}

func TestArtifactService_Put_ContentHash_IsSHA256(t *testing.T) {
	data := []byte("hash me")
	a, err := newSvc(newStubRepo(), newStubS3()).Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp", Filename: "f.txt", ContentType: "text/plain", Data: data,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := sha256hex(data)
	if a.ContentHash != want {
		t.Errorf("want hash %s, got %s", want, a.ContentHash)
	}
}

func TestArtifactService_Put_EmptyData_Errors(t *testing.T) {
	_, err := newSvc(newStubRepo(), newStubS3()).Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp", Filename: "f.txt", ContentType: "text/plain", Data: nil,
	})
	if err == nil {
		t.Fatal("expected error for empty data, got nil")
	}
}

func TestArtifactService_Put_MaxInlineBytes_Boundary(t *testing.T) {
	// MaxInlineBytes-1 bytes must route to inline (db), not S3.
	data := make([]byte, models.MaxInlineBytes-1)
	a, err := newSvc(newStubRepo(), newStubS3()).Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp", Filename: "f.bin", ContentType: "application/octet-stream", Data: data,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if a.StoredIn() != "db" {
		t.Errorf("MaxInlineBytes-1 must be stored in db, got %s", a.StoredIn())
	}
}

func TestArtifactService_Put_S3Key_ContainsHashAndFilename(t *testing.T) {
	s3 := newStubS3()
	data := []byte(strings.Repeat("y", models.MaxInlineBytes+1))
	a, err := newSvc(newStubRepo(), s3).Put(context.Background(), service.ArtifactPutRequest{
		UserID: "uid", DatapackID: "dpid", Filename: "report.pdf",
		ContentType: "application/pdf", Data: data,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.Contains(a.S3Key, sha256hex(data)) {
		t.Errorf("S3Key should contain content hash; got %s", a.S3Key)
	}
	if !strings.Contains(a.S3Key, "report.pdf") {
		t.Errorf("S3Key should contain filename; got %s", a.S3Key)
	}
}

// ---------------------------------------------------------------------------
// Tests — Get
// ---------------------------------------------------------------------------

func TestArtifactService_Get_InlineArtifact_ReturnsBase64(t *testing.T) {
	repo := newStubRepo()
	svc := newSvc(repo, newStubS3())

	data := []byte("get me back")
	a, _ := svc.Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp", Filename: "f.txt", ContentType: "text/plain", Data: data,
	})

	resp, err := svc.Get(context.Background(), "dp", a.ContentHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response, got nil")
	}
	if resp.ContentBase64 == "" {
		t.Error("expected content_base64 for inline artifact")
	}
	if len(resp.Content) != 0 {
		t.Error("inline artifact must not set Content field")
	}
}

func TestArtifactService_Get_S3Artifact_ReturnsBytes(t *testing.T) {
	repo := newStubRepo()
	s3 := newStubS3()
	svc := newSvc(repo, s3)

	data := []byte(strings.Repeat("z", models.MaxInlineBytes+1))
	a, _ := svc.Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp", Filename: "large.bin", ContentType: "application/octet-stream", Data: data,
	})

	resp, err := svc.Get(context.Background(), "dp", a.ContentHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response, got nil")
	}
	if len(resp.Content) == 0 {
		t.Error("expected Content bytes for S3 artifact")
	}
	if resp.ContentBase64 != "" {
		t.Error("S3 artifact must not set ContentBase64 field")
	}
}

func TestArtifactService_Get_NotFound_ReturnsNil(t *testing.T) {
	resp, err := newSvc(newStubRepo(), newStubS3()).Get(context.Background(), "dp", "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Errorf("expected nil for missing artifact, got %+v", resp)
	}
}

func TestArtifactService_Get_ExpiredArtifact_ReturnsError(t *testing.T) {
	repo := newStubRepo()
	svc := newSvc(repo, newStubS3())

	data := []byte("soon expired")
	a, _ := svc.Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp", Filename: "x.txt", ContentType: "text/plain", Data: data,
	})

	now := time.Now()
	repo.rows["dp:"+a.ContentHash].ExpiredAt = &now

	_, err := svc.Get(context.Background(), "dp", a.ContentHash)
	if err == nil {
		t.Fatal("expected error for expired artifact, got nil")
	}
	if err.Error() != "artifact expired" {
		t.Errorf("expected 'artifact expired', got %q", err.Error())
	}
}

func TestArtifactService_Get_DifferentDatapacks_Isolated(t *testing.T) {
	svc := newSvc(newStubRepo(), newStubS3())

	data := []byte("shared bytes")
	a, _ := svc.Put(context.Background(), service.ArtifactPutRequest{
		UserID: "u", DatapackID: "dp-A", Filename: "f.txt", ContentType: "text/plain", Data: data,
	})

	// Same hash in dp-B must not find dp-A's artifact
	resp, err := svc.Get(context.Background(), "dp-B", a.ContentHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Error("artifact from dp-A must not be visible in dp-B")
	}
}
