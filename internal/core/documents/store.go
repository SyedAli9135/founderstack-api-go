// Package documents is workflow 6's RAG pipeline: S3-backed file
// storage, PDF/DOCX/TXT/MD text extraction, chunking, and the
// Cohere-embed → Pinecone-upsert indexing step, plus the background
// processing/purge jobs and boot-time recovery sweep that drive it.
package documents

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Store wraps the S3 (or LocalStack, in dev) operations this package
// needs — upload on submit, download for processing, delete on purge.
type Store struct {
	client *s3.Client
	bucket string
}

// NewStore builds a Store. endpointURL empty hits real AWS; set (e.g.
// LocalStack's http://localhost:4566) it switches to path-style
// addressing, which LocalStack requires and real AWS doesn't.
func NewStore(ctx context.Context, region, accessKeyID, secretAccessKey, endpointURL, bucket string) (*Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("documents: load aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpointURL != "" {
			o.BaseEndpoint = aws.String(endpointURL)
			o.UsePathStyle = true
		}
	})

	return &Store{client: client, bucket: bucket}, nil
}

// Key builds the S3 object key: documents/{org_id}/{doc_id}/{filename} —
// namespaced by org so two orgs uploading a same-named file never collide.
func Key(orgID, docID, filename string) string {
	return fmt.Sprintf("documents/%s/%s/%s", orgID, docID, filename)
}

func (s *Store) Upload(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("documents: s3 upload %s: %w", key, err)
	}
	return nil
}

// Download returns the object body — caller must Close() it.
func (s *Store) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("documents: s3 download %s: %w", key, err)
	}
	return out.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("documents: s3 delete %s: %w", key, err)
	}
	return nil
}
