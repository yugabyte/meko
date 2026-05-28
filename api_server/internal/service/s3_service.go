package service

import (
	"context"
	"errors"
	"fmt"
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
	AuthMode  S3AuthMode
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
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

	switch cfg.AuthMode {
	case S3AuthMinio:
		if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Endpoint == "" {
			return nil, fmt.Errorf("minio mode requires endpoint, access-key and secret-key")
		}
		endpoint := cfg.Endpoint
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			endpoint = "http://" + endpoint
		}
		client = s3.New(s3.Options{
			Region:       cfg.Region,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			UsePathStyle: true,
			BaseEndpoint: aws.String(strings.TrimSuffix(endpoint, "/")),
		})
		slog.Info("s3 service initialized", "mode", "minio",
			"endpoint", cfg.Endpoint, "bucket", cfg.Bucket, "region", cfg.Region)

	case S3AuthIAMUser:
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("iam-user mode requires access-key and secret-key")
		}
		client = s3.New(s3.Options{
			Region:      cfg.Region,
			Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		})
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
		slog.Info("s3 service initialized", "mode", "iam-role",
			"bucket", cfg.Bucket, "region", cfg.Region)

	default:
		return nil, fmt.Errorf("unknown s3-auth-mode: %q (valid: minio, iam-user, iam-role)", cfg.AuthMode)
	}

	return &S3Service{
		client:        client,
		presignClient: s3.NewPresignClient(client),
		bucket:        cfg.Bucket,
	}, nil
}

func (s *S3Service) Enabled() bool {
	return s != nil && s.client != nil
}

func (s *S3Service) Bucket() string {
	return s.bucket
}

type PresignedUpload struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// GenerateUploadURL builds a presigned PUT URL for uploading a file to
// the per-user, per-datapack S3 prefix. The resulting key is
// "<userID>/<datapackID>/<filename>" so each user owns a top-level S3
// namespace and each datapack a sub-folder underneath it.
func (s *S3Service) GenerateUploadURL(
	ctx context.Context,
	userID, datapackID, filename, contentType string,
) (*PresignedUpload, error) {
	key := fmt.Sprintf("%s/%s/%s", userID, datapackID, sanitizeFilename(filename))

	exists, err := s.ObjectExists(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("check duplicate: %w", err)
	}
	if exists {
		return nil, ErrDuplicateFile
	}

	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}

	presigned, err := s.presignClient.PresignPutObject(ctx, input,
		s3.WithPresignExpires(5*time.Minute),
	)
	if err != nil {
		return nil, fmt.Errorf("presign put: %w", err)
	}

	return &PresignedUpload{
		URL: presigned.URL,
		Key: key,
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

// ErrDuplicateFile is returned when a file with the same name already exists.
var ErrDuplicateFile = fmt.Errorf("file with the same name already exists")

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

// S3Object holds metadata for a single object in S3.
type S3Object struct {
	Key         string // full S3 key e.g. "<user_id>/<datapack_id>/report.pdf"
	ETag        string // S3 ETag (MD5 for non-multipart uploads)
	ContentType string // MIME type derived from file extension
	// Size is the object size in bytes (S3 ListObjectsV2 Contents.Size).
	// Zero on the rare paths where S3 doesn't return a size; callers
	// should treat 0 as "unknown" rather than "empty file".
	Size int64
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
	".mp4":  "video/mp4",
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
			objects = append(objects, S3Object{
				Key:         key,
				ETag:        etag,
				ContentType: mimeTypeFromKey(key),
				Size:        size,
			})
		}
	}
	return objects, nil
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
