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

	S3UserUploadsPrefix = "user-uploads"
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

func (s *S3Service) GenerateUploadURL(
	ctx context.Context,
	accountID, datapackID, filename, contentType string,
) (*PresignedUpload, error) {
	key := fmt.Sprintf("%s/%s/%s/%s", S3UserUploadsPrefix, accountID, datapackID, sanitizeFilename(filename))

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

// ErrDuplicateFile is returned when a file with the same name already exists.
var ErrDuplicateFile = fmt.Errorf("file with the same name already exists")

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
	Key  string // full S3 key e.g. "accountId/datapackId/1776163643189_report.pdf"
	ETag string // S3 ETag (MD5 for non-multipart uploads)
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
			objects = append(objects, S3Object{
				Key:  aws.ToString(obj.Key),
				ETag: etag,
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
