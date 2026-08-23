package documents

import (
	"context"
	"fmt"
	"io"

	cohere "github.com/cohere-ai/cohere-go/v2"
	coherecli "github.com/cohere-ai/cohere-go/v2/client"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
)

// BlobStore, Embedder, and VectorIndex are Processor's 3 external
// dependencies, cut down to just the methods it actually calls — the same
// interface-segregation reasoning `internal/core/integrations` documents
// for its Provider/OAuthProvider/... split. *Store already satisfies
// BlobStore structurally (no adapter needed); Embedder and VectorIndex wrap
// the Cohere and Pinecone SDK clients, whose concrete types can't stand in
// for a hand-written interface directly (WithNamespace returns a concrete
// *pinecone.IndexConnection, not an interface). Splitting these out is
// what lets processor_integration_test.go and
// internal/api/documents/handler_integration_test.go run the real
// upload/process/purge/reindex pipeline against fakes — no live S3,
// Cohere, or Pinecone dependency, and no LocalStack service or third-party
// API keys needed in CI.
type BlobStore interface {
	Upload(ctx context.Context, key string, body io.Reader, contentType string) error
	Download(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

type VectorIndex interface {
	Namespace(ns string) VectorIndex
	Upsert(ctx context.Context, vectors []*pinecone.Vector) error
	DeleteByID(ctx context.Context, ids []string) error
}

type cohereEmbedder struct {
	client *coherecli.Client
}

// NewCohereEmbedder adapts a real Cohere client to the Embedder interface
// Processor depends on. The client passed in must be built with
// option.WithMaxAttempts set high enough to survive a rate-limit window —
// see cmd/api/main.go::newDocumentsProcessor's comment for why. cohere-go's
// client already retries 429/408/5xx internally with jittered exponential
// backoff and Retry-After support (internal/retrier.go) — an outer retry
// loop here would just be a worse duplicate of that, not an improvement.
func NewCohereEmbedder(client *coherecli.Client) Embedder {
	return cohereEmbedder{client: client}
}

func (e cohereEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	inputType := cohere.EmbedInputTypeSearchDocument
	resp, err := e.client.Embed(ctx, &cohere.EmbedRequest{
		Texts:     texts,
		Model:     stringPtr(embedModel),
		InputType: &inputType,
	})
	if err != nil {
		return nil, err
	}
	floats := resp.GetEmbeddingsFloats()
	if floats == nil {
		return nil, fmt.Errorf("cohere response had no float embeddings")
	}
	return floats.Embeddings, nil
}

func stringPtr(s string) *string { return &s }

type pineconeIndex struct {
	conn *pinecone.IndexConnection
}

// NewPineconeIndex adapts a real Pinecone index connection to the
// VectorIndex interface Processor depends on.
func NewPineconeIndex(conn *pinecone.IndexConnection) VectorIndex {
	return pineconeIndex{conn: conn}
}

func (i pineconeIndex) Namespace(ns string) VectorIndex {
	return pineconeIndex{conn: i.conn.WithNamespace(ns)}
}

func (i pineconeIndex) Upsert(ctx context.Context, vectors []*pinecone.Vector) error {
	_, err := i.conn.UpsertVectors(ctx, vectors)
	return err
}

func (i pineconeIndex) DeleteByID(ctx context.Context, ids []string) error {
	return i.conn.DeleteVectorsById(ctx, ids)
}
