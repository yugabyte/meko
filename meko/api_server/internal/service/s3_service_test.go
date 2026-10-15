package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestReadObjectHead_EmptyObject_NoErr pins the fix for a zero-byte object: a
// Range GET against it is unsatisfiable and S3/MinIO answer 416 InvalidRange,
// which must surface as an empty read (nil, nil), not an error -- otherwise
// VerifyUploadedBytes' own "this file is empty" rejection is unreachable and
// ConfirmUpload returns a 500 instead of the intended 400 for an empty upload.
func TestReadObjectHead_EmptyObject_NoErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidRange</Code><Message>The requested range is not satisfiable</Message></Error>`))
	}))
	defer srv.Close()

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("k", "s", ""),
		UsePathStyle: true,
		BaseEndpoint: aws.String(srv.URL),
	})
	svc := &S3Service{client: client, bucket: "test-bucket"}

	head, err := svc.ReadObjectHead(context.Background(), "u/dp/hash/empty.txt", 3072)
	if err != nil {
		t.Fatalf("ReadObjectHead: expected nil error for InvalidRange, got %v", err)
	}
	if len(head) != 0 {
		t.Fatalf("ReadObjectHead: expected empty head, got %d bytes", len(head))
	}
}
