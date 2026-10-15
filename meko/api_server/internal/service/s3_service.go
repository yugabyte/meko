package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3AuthMode controls how the S3 client authenticates.
//   - "minio"    → static credentials + custom endpoint + path-style
//   - "iam-user" → static credentials + AWS virtual-hosted style
//   - "iam-role" → AWS default credential chain (IRSA / instance profile)
type S3AuthMode string

const (
	S3AuthMinio   S3AuthMode = "minio"
	S3AuthIAMUser S3AuthMode = "iam-user"
	S3AuthIAMRole S3AuthMode = "iam-role"
)

type S3Config struct {
	AuthMode S3AuthMode
	Endpoint string
	// PublicEndpoint, when set, is used ONLY for presigned URLs (upload /
	// download) so the host/port baked into the SigV4 signature is one the
	// end-user's browser can reach. Server-side calls (HeadObject,
	// DeleteObject, ListObjects) keep using Endpoint. This solves the
	// split-horizon case where api_server reaches MinIO at an internal
	// docker hostname (http://minio:9000) but the browser must use the
	// published host port (http://localhost:19090). Empty = presign with
	// the same Endpoint as everything else (the original behaviour).
	PublicEndpoint string
	AccessKey      string
	SecretKey      string
	Bucket         string
	Region         string
}

type S3Service struct {
	client        *s3.Client
	presignClient *s3.PresignClient
	bucket        string
}

func NewS3Service(cfg S3Config) (*S3Service, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}

	var client *s3.Client
	// presignSource is the client whose endpoint the presigned URLs are
	// signed against. Defaults to the main client; overridden below when a
	// public endpoint is configured so browser-facing URLs carry a
	// host/port the browser can actually reach.
	var presignSource *s3.Client

	normalizeEndpoint := func(ep string) string {
		if !strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://") {
			ep = "http://" + ep
		}
		return strings.TrimSuffix(ep, "/")
	}

	switch cfg.AuthMode {
	case S3AuthMinio:
		if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Endpoint == "" {
			return nil, fmt.Errorf("minio mode requires endpoint, access-key and secret-key")
		}
		endpoint := normalizeEndpoint(cfg.Endpoint)
		creds := credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
		client = s3.New(s3.Options{
			Region:       cfg.Region,
			Credentials:  creds,
			UsePathStyle: true,
			BaseEndpoint: aws.String(endpoint),
		})
		presignSource = client
		if cfg.PublicEndpoint != "" {
			publicEndpoint := normalizeEndpoint(cfg.PublicEndpoint)
			presignSource = s3.New(s3.Options{
				Region:       cfg.Region,
				Credentials:  creds,
				UsePathStyle: true,
				BaseEndpoint: aws.String(publicEndpoint),
			})
			slog.Info("s3 presign client uses public endpoint",
				"public_endpoint", publicEndpoint)
		}
		slog.Info("s3 service initialized", "mode", "minio",
			"endpoint", cfg.Endpoint, "public_endpoint", cfg.PublicEndpoint,
			"bucket", cfg.Bucket, "region", cfg.Region)

	case S3AuthIAMUser:
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("iam-user mode requires access-key and secret-key")
		}
		client = s3.New(s3.Options{
			Region:      cfg.Region,
			Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		})
		presignSource = client
		slog.Info("s3 service initialized", "mode", "iam-user",
			"bucket", cfg.Bucket, "region", cfg.Region)

	case S3AuthIAMRole:
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Region),
		)
		if err != nil {
			return nil, fmt.Errorf("load AWS default config: %w", err)
		}
		client = s3.NewFromConfig(awsCfg)
		presignSource = client
		slog.Info("s3 service initialized", "mode", "iam-role",
			"bucket", cfg.Bucket, "region", cfg.Region)

	default:
		return nil, fmt.Errorf("unknown s3-auth-mode: %q (valid: minio, iam-user, iam-role)", cfg.AuthMode)
	}

	return &S3Service{
		client:        client,
		presignClient: s3.NewPresignClient(presignSource),
		bucket:        cfg.Bucket,
	}, nil
}

func (s *S3Service) Enabled() bool {
	return s != nil && s.client != nil
}

func (s *S3Service) Bucket() string {
	return s.bucket
}

// UploadURLTTL is how long an issued upload authorization stays valid.
// It bounds when the POST may *start*; a transfer already underway when it
// lapses is not cut off, so anything deriving a "the bytes can never arrive
// now" deadline must add its own slack on top of this.
const UploadURLTTL = 5 * time.Minute

type PresignedUpload struct {
	URL string `json:"url"`
	// Fields are the multipart form fields the POST must carry, verbatim and
	// before the file part: the signed policy, the signature, the credential
	// scope and the key. Empty when AlreadyUploaded.
	Fields map[string]string `json:"fields,omitempty"`
	Key    string            `json:"key"`
	// AlreadyUploaded is true when identical content already exists in this
	// datapack (matched by content hash, regardless of filename). The
	// presigned URL is empty in that case — the client should skip the upload
	// and proceed directly to ConfirmUpload so the document gets indexed.
	AlreadyUploaded bool `json:"already_uploaded"`
	// ExistingFilename is the name the deduplicated content was first stored
	// under, set only when AlreadyUploaded is true. The client uses it to tell
	// the user whether the skipped file was the same name or just same bytes.
	ExistingFilename string `json:"existing_filename,omitempty"`
	// StatusURL is where the caller polls this one file's progress, from "the
	// bytes have not arrived" through to indexed. Filled in by the handler,
	// since the URL depends on the request.
	StatusURL string `json:"status_url,omitempty"`
}

// sha256HexToChecksum converts a lowercase-hex SHA-256 (the wire format
// content_hash arrives in) to the base64 form S3's checksum condition and
// HeadObject response use.
func sha256HexToChecksum(hexHash string) (string, error) {
	raw, err := hex.DecodeString(hexHash)
	if err != nil {
		return "", fmt.Errorf("content hash is not valid hex: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// contentHashFromKey extracts the hex SHA-256 segment GenerateUploadURL
// embeds in every key it signs ("<user_id>/<datapack_id>/<hash>/<filename>"),
// letting a caller holding only the key -- the pending-upload watcher,
// notably -- recover what content was declared without a dedicated column.
func contentHashFromKey(key string) (hash string, ok bool) {
	parts := strings.SplitN(key, "/", 4)
	if len(parts) != 4 {
		return "", false
	}
	return parts[2], true
}

// GenerateUploadURL authorizes a direct browser/agent upload to
// "<userID>/<datapackID>/<sha256>/<filename>". Dedup matches on the
// "<sha256>/" prefix, not the full key, so identical content under any
// filename reports AlreadyUploaded=true instead of uploading again.
//
// A presigned **POST**, whose policy holds the body to exactly size bytes
// (content-length-range [size, size]): S3 rejects any other length with 400
// EntityTooLarge or EntityTooSmall before the object is created. Exact, not a
// range up to the cap, because the declared size is what the storage quota
// counts an upload by while it is in flight, and S3 holding the body to it is
// what makes that count true. maxSizeBytes is the most size may be.
//
// contentHash also goes into the policy as a required x-amz-checksum-sha256
// field, so S3 itself verifies the uploaded bytes hash to the value the
// caller claimed — otherwise contentHash is just a string the client could
// lie about (to defeat the dedup check above, or to land bytes under a
// hash-prefixed key that doesn't match their real content) with nothing
// after presign time to catch it. S3's checksum fields are base64, not hex,
// so contentHash (already validated as 64 hex chars by the caller) is
// re-encoded before use.
//
// Content-Type is deliberately unconstrained — S3 never inspects the bytes,
// so a condition on it would only restrict what the client claims, for no
// gain. The extension is what's pinned, and S3Object.ContentType derives
// from it via mimeTypeFromKey, never read back from the stored object.
func (s *S3Service) GenerateUploadURL(
	ctx context.Context,
	userID, datapackID, filename, contentHash string,
	size, maxSizeBytes int64,
) (*PresignedUpload, error) {
	existing, err := s.ExistingUpload(ctx, userID, datapackID, contentHash)
	if err != nil || existing != nil {
		return existing, err
	}
	return s.SignUploadURL(ctx, UploadKey(userID, datapackID, contentHash, filename), contentHash, size, maxSizeBytes)
}

// UploadKey is the key GenerateUploadURL signs a fresh upload for.
func UploadKey(userID, datapackID, contentHash, filename string) string {
	return uploadHashPrefix(userID, datapackID, contentHash) + sanitizeFilename(filename)
}

func uploadHashPrefix(userID, datapackID, contentHash string) string {
	return fmt.Sprintf("%s/%s/%s/", userID, datapackID, contentHash)
}

// ExistingUpload is GenerateUploadURL's dedup step on its own: the
// AlreadyUploaded answer when identical content is already stored in the
// datapack, else nil. A caller that checks a quota does it after this, since
// an upload that sends no bytes needs no room.
func (s *S3Service) ExistingUpload(
	ctx context.Context, userID, datapackID, contentHash string,
) (*PresignedUpload, error) {
	existingKey, err := s.firstObjectKeyUnderPrefix(ctx, uploadHashPrefix(userID, datapackID, contentHash))
	if err != nil {
		return nil, fmt.Errorf("check existing upload: %w", err)
	}
	if existingKey == "" {
		return nil, nil
	}
	// Identical content already stored (under existingKey's filename).
	return &PresignedUpload{
		Key:              existingKey,
		AlreadyUploaded:  true,
		ExistingFilename: path.Base(existingKey),
	}, nil
}

// SignUploadURL is GenerateUploadURL's signing step on its own, for a key from
// UploadKey whose content ExistingUpload found no copy of.
func (s *S3Service) SignUploadURL(
	ctx context.Context, key, contentHash string, size, maxSizeBytes int64,
) (*PresignedUpload, error) {
	if maxSizeBytes <= 0 || maxSizeBytes > maxUploadFileBytes {
		maxSizeBytes = maxUploadFileBytes
	}
	if size <= 0 || size > maxSizeBytes {
		return nil, fmt.Errorf("upload size %d is outside 1..%d", size, maxSizeBytes)
	}

	base64OfSHA256, err := sha256HexToChecksum(contentHash)
	if err != nil {
		return nil, err
	}

	// PresignPostObject builds the returned Fields itself (algorithm,
	// credential, date, signature, key, policy) and does not look at any
	// other PutObjectInput field, so the checksum has to be added by hand on
	// both sides: as a policy condition (what S3 checks the upload against)
	// and as a form field (what tells the client to actually send it).
	presignedPostURL, err := s.presignClient.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignPostOptions) {
		o.Expires = UploadURLTTL
		o.Conditions = []any{
			[]any{"content-length-range", size, size},
			[]any{"eq", "$x-amz-checksum-sha256", base64OfSHA256},
		}
	})
	if err != nil {
		return nil, fmt.Errorf("presign post: %w", err)
	}
	presignedPostURL.Values["x-amz-checksum-sha256"] = base64OfSHA256

	return &PresignedUpload{
		URL:    presignedPostURL.URL,
		Fields: presignedPostURL.Values,
		Key:    key,
	}, nil
}

// PresignedDownload is the response shape for short-lived presigned
// GET URLs used by the SharedKnowledge "Download" action.
type PresignedDownload struct {
	URL       string `json:"url"`
	Key       string `json:"key"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// GenerateDownloadURL builds a presigned GET URL for the given S3 key.
// Caller is responsible for verifying that the key belongs to the
// caller's datapack (a presigned URL on an arbitrary key would be a
// confused-deputy hole). See KnowledgeBaseHandler.RequestDownloadURL
// for the ownership check.
//
// 5-minute expiry matches the upload-URL expiry — long enough for the
// browser to follow the redirect immediately, short enough that the
// URL can't be passed around indefinitely.
func (s *S3Service) GenerateDownloadURL(
	ctx context.Context,
	key string,
) (*PresignedDownload, error) {
	const expiry = 5 * time.Minute
	// Extract the basename from the key for the Content-Disposition filename.
	filename := path.Base(key)
	// SECURITY: force Content-Disposition=attachment on every download so the
	// browser saves the file instead of rendering it inline. Knowledge bases
	// accept HTML/HTM (and other text) uploads, and inline rendering of an
	// attacker-supplied .html would execute its scripts in our origin — a
	// stored XSS. attachment neutralizes that regardless of the object's
	// stored Content-Type. Keep this unconditional; do not special-case types.
	input := &s3.GetObjectInput{
		Bucket:                     aws.String(s.bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String(`attachment; filename="` + filename + `"`),
	}
	presigned, err := s.presignClient.PresignGetObject(ctx, input,
		s3.WithPresignExpires(expiry),
	)
	if err != nil {
		return nil, fmt.Errorf("presign get %s: %w", key, err)
	}
	return &PresignedDownload{
		URL:       presigned.URL,
		Key:       key,
		ExpiresIn: int(expiry.Seconds()),
	}, nil
}

// DeleteObject removes a single object from the bucket. Used by the
// per-file DELETE handler after the database-side cleanup succeeds.
// S3 DeleteObject is idempotent: deleting a missing key returns
// success, so the handler doesn't need to special-case "already gone".
func (s *S3Service) DeleteObject(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %s: %w", key, err)
	}
	return nil
}

// firstObjectKeyUnderPrefix returns the key of any one object under prefix, or
// "" if none exist. It requests at most one key (MaxKeys=1), which is all the
// content-hash dedup check needs: it only cares whether an object with this
// hash already exists, not how many or their metadata.
func (s *S3Service) firstObjectKeyUnderPrefix(ctx context.Context, prefix string) (string, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return "", fmt.Errorf("list objects prefix=%s: %w", prefix, err)
	}
	if len(out.Contents) == 0 {
		return "", nil
	}
	return aws.ToString(out.Contents[0].Key), nil
}

// ObjectExists checks whether an object with the given key exists in the bucket.
func (s *S3Service) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NotFound
		if errors.As(err, &nsk) {
			return false, nil
		}
		return false, fmt.Errorf("head object %s: %w", key, err)
	}
	return true, nil
}

// ErrObjectNotFound reports that the bucket has no such key — as distinct from
// not being able to find out. The pending-upload watcher turns on that
// difference: absent is a statement about the upload, while a 5xx, a timeout or
// an AccessDenied says only that S3 wouldn't answer, and treating the second as
// the first would discard good uploads over a network blip or an IAM regression.
var ErrObjectNotFound = errors.New("object not found")

// HeadObject returns an object's metadata, wrapping ErrObjectNotFound when the
// key isn't there. Metadata only: no bytes, no egress, and identical on MinIO.
//
// Size here is the authoritative one. On the presigned path it is the only size
// api_server ever sees, since the bytes go straight to the bucket and the
// client's declared size is just a claim.
func (s *S3Service) HeadObject(ctx context.Context, key string) (S3Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		// Without this, S3 omits ChecksumSHA256 from the response even
		// though the object was stored with one -- see ChecksumSHA256 below.
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		var nsk *types.NotFound
		if errors.As(err, &nsk) {
			return S3Object{}, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return S3Object{}, fmt.Errorf("head object %s: %w", key, err)
	}
	etag := ""
	if out.ETag != nil {
		etag = strings.Trim(*out.ETag, "\"")
	}
	var uploadedAt time.Time
	if out.LastModified != nil {
		uploadedAt = *out.LastModified
	}
	return S3Object{
		Key:            key,
		ETag:           etag,
		ContentType:    mimeTypeFromKey(key),
		Size:           aws.ToInt64(out.ContentLength),
		UploadedAt:     uploadedAt,
		ChecksumSHA256: aws.ToString(out.ChecksumSHA256),
	}, nil
}

// S3Object holds metadata for a single object in S3.
type S3Object struct {
	Key         string // full S3 key e.g. "<user_id>/<datapack_id>/report.pdf"
	ETag        string // S3 ETag (MD5 for non-multipart uploads)
	ContentType string // MIME type derived from file extension
	// Size is the object size in bytes (S3 ListObjectsV2 Contents.Size).
	// Zero on the rare paths where S3 doesn't return a size; callers
	// should treat 0 as "unknown" rather than "empty file".
	Size int64
	// UploadedAt is the S3 LastModified timestamp — the moment the object
	// was written to the bucket. Zero value when S3 omits it (shouldn't
	// happen in practice).
	UploadedAt time.Time
	// ChecksumSHA256 is the base64 SHA-256 S3 stored for the object, present
	// only when the uploader supplied one -- every presigned-POST upload
	// through GenerateUploadURL does. Empty when the backend doesn't return
	// it even with ChecksumMode enabled; callers must treat empty as
	// "unverifiable", not "mismatch".
	ChecksumSHA256 string
}

// mimeTypeByExtension maps common file extensions to MIME types supported by
// the dist_rag worker. Falls back to "application/octet-stream" for unknown
// extensions so such files are inserted but skipped by the worker filter.
var mimeTypeByExtension = map[string]string{
	".txt":  "text/plain",
	".text": "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".xml":  "text/xml",
	".html": "text/html",
	".htm":  "text/html",
	".json": "application/json",
	".pdf":  "application/pdf",
}

// mimeTypeFromKey derives the MIME type for an S3 key based on its extension.
func mimeTypeFromKey(key string) string {
	ext := strings.ToLower(path.Ext(key))
	if mt, ok := mimeTypeByExtension[ext]; ok {
		return mt
	}
	return "application/octet-stream"
}

// ListObjects returns all objects under the given key prefix.
func (s *S3Service) ListObjects(ctx context.Context, prefix string) ([]S3Object, error) {
	var objects []S3Object
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	}

	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects prefix=%s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			etag := ""
			if obj.ETag != nil {
				etag = strings.Trim(*obj.ETag, "\"")
			}
			key := aws.ToString(obj.Key)
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			var uploadedAt time.Time
			if obj.LastModified != nil {
				uploadedAt = *obj.LastModified
			}
			objects = append(objects, S3Object{
				Key:         key,
				ETag:        etag,
				ContentType: mimeTypeFromKey(key),
				Size:        size,
				UploadedAt:  uploadedAt,
			})
		}
	}
	return objects, nil
}

// UploadArtifactObject writes under the "artifacts/" prefix — disjoint from
// GenerateUploadURL's KB key space, so nothing that walks or dedups on
// "<userID>/<datapackID>/" ever reaches an artifact. Artifacts stored
// before this method existed still live at the old, shared-prefix key until
// migrated — see meko/scripts/migrate-artifacts-prefix/migrate.py.
func (s *S3Service) UploadArtifactObject(
	ctx context.Context,
	userID, datapackID, contentHash, filename, contentType string,
	data []byte,
) (string, error) {
	key := fmt.Sprintf("artifacts/%s/%s/%s/%s", userID, datapackID, contentHash, sanitizeFilename(filename))
	return key, s.putObject(ctx, key, contentType, data)
}

func (s *S3Service) putObject(ctx context.Context, key, contentType string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
		Body:        bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

// DownloadObject fetches the bytes for key from S3 and returns them.
func (s *S3Service) DownloadObject(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read object %s: %w", key, err)
	}
	return data, nil
}

// apiErrorCoder matches the ErrorCode() method every AWS SDK v2 API error
// implements, without importing smithy-go directly: errors.As matches an
// interface target structurally, so this local interface is enough to pull
// the code out of whatever concrete error type the SDK returns.
type apiErrorCoder interface {
	ErrorCode() string
}

// ReadObjectHead fetches at most n leading bytes of an object.
//
// A ranged GET, so the transfer is bounded by n no matter how large the object
// is — the point being that content can be verified on the presigned upload
// path, where api_server never sees the bytes, without pulling a whole file
// into memory to look at its first three kilobytes.
//
// A range that overshoots a short object is clamped by S3 rather than being an
// error, so a 33-byte object simply returns 33 bytes -- except at zero: a
// range against an empty object has nothing to clamp to, so S3/MinIO answer
// 416 InvalidRange instead. That's reported as a nil, nil empty read here,
// not an error, so a zero-byte object surfaces through VerifyUploadedBytes'
// own "this file is empty" check rather than as an opaque S3 read failure.
func (s *S3Service) ReadObjectHead(
	ctx context.Context, key string, n int64,
) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("read head %s: n must be positive", key)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=0-%d", n-1)),
	})
	if err != nil {
		// InvalidRange isn't a typed error like types.NotFound above — it's
		// a generic validation error, so apiErrorCoder is what catches it.
		var apiErr apiErrorCoder
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidRange" {
			return nil, nil
		}
		return nil, fmt.Errorf("range get %s: %w", key, err)
	}
	defer out.Body.Close()

	// LimitReader as well as the Range header: the range is what keeps the
	// transfer small, this is the guarantee that a server ignoring it can't
	// hand us an unbounded body.
	head, err := io.ReadAll(io.LimitReader(out.Body, n))
	if err != nil {
		return nil, fmt.Errorf("read head %s: %w", key, err)
	}
	return head, nil
}

// sanitizeFilename replaces invalid characters in a filename with underscores which is accepted by S3.
func sanitizeFilename(name string) string {
	base := path.Base(name)
	base = strings.ReplaceAll(base, " ", "_")
	base = strings.Map(func(r rune) rune {
		if (!unicode.IsLetter(r) && !unicode.IsDigit(r)) &&
			!(r == '_' || r == '-' || r == '.') {
			return '_'
		}
		return r
	}, base)
	return base
}
