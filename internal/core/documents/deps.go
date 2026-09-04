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
// dependencies, cut to just the methods it calls — same interface
// segregation as internal/core/integrations' Provider split. *Store
// already satisfies BlobStore structurally; Embedder/VectorIndex wrap the
// Cohere/Pinecone SDK clients, whose concrete types can't stand in
// directly (WithNamespace returns a concrete type, not an interface).
// This is what lets the integration tests run the real pipeline against
// fakes — no live S3/Cohere/Pinecone or LocalStack needed in CI.
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

// NewCohereEmbedder adapts a real Cohere client to the Embedder interface.
// The client must be built with option.WithMaxAttempts set high enough to
// survive a rate-limit window (see newDocumentsProcessor) — cohere-go
// already retries 429/408/5xx with jittered backoff internally, so an
// outer retry loop here would just duplicate it, worse.
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
