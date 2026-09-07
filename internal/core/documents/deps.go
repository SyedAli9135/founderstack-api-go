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
	// EmbedOne embeds a single text for search (workflow 12) — mode picks
	// Cohere's asymmetric query-vs-document input type, distinct from
	// Embed's ingestion-only EmbedInputTypeSearchDocument.
	EmbedOne(ctx context.Context, text string, mode EmbedMode) ([]float64, error)
}

// EmbedMode is this package's own type (not cohere.EmbedInputType) so the
// Embedder interface stays SDK-agnostic — same reasoning VectorIndex
// already follows for Upsert/DeleteByID.
type EmbedMode int

const (
	EmbedModeQuery    EmbedMode = iota // the founder's literal search text
	EmbedModeDocument                  // HyDE's hypothetical-answer text, embedded like a real indexed chunk
)

type VectorIndex interface {
	Namespace(ns string) VectorIndex
	Upsert(ctx context.Context, vectors []*pinecone.Vector) error
	DeleteByID(ctx context.Context, ids []string) error
	// Query returns candidates ordered by Pinecone's own score (descending)
	// — Searcher may rerank them further, or use this ordering as-is.
	Query(ctx context.Context, vector []float32, topK uint32, filter *pinecone.MetadataFilter) ([]QueryMatch, error)
}

// QueryMatch is one Pinecone match, with metadata already decoded out of
// structpb so callers never need to import it.
type QueryMatch struct {
	ID       string
	Score    float64
	Metadata map[string]any // doc_id (string), chunk_index (float64 — JSON numbers), text (string)
}

// Reranker cross-encodes query against each candidate doc's text —
// meaningfully more accurate than the bi-encoder similarity Pinecone's
// vector search alone provides, at the cost of one more network call.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankMatch, error)
}

// RerankMatch.Index refers back into the docs slice Rerank was called
// with, not into any earlier ordering.
type RerankMatch struct {
	Index          int
	RelevanceScore float64
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

func (e cohereEmbedder) EmbedOne(ctx context.Context, text string, mode EmbedMode) ([]float64, error) {
	inputType := cohere.EmbedInputTypeSearchQuery
	if mode == EmbedModeDocument {
		inputType = cohere.EmbedInputTypeSearchDocument
	}
	resp, err := e.client.Embed(ctx, &cohere.EmbedRequest{
		Texts:     []string{text},
		Model:     stringPtr(embedModel),
		InputType: &inputType,
	})
	if err != nil {
		return nil, err
	}
	floats := resp.GetEmbeddingsFloats()
	if floats == nil || len(floats.Embeddings) == 0 {
		return nil, fmt.Errorf("cohere response had no float embeddings")
	}
	return floats.Embeddings[0], nil
}

func stringPtr(s string) *string { return &s }

// rerankModel: Cohere's current cross-encoder rerank model, per
const rerankModel = "rerank-v3.5"

type cohereReranker struct {
	client *coherecli.Client
}

// NewCohereReranker adapts a real Cohere client to the Reranker interface.
// Pass the same client instance NewCohereEmbedder wraps — no reason to pay
// for a second connection/retry-policy setup.
func NewCohereReranker(client *coherecli.Client) Reranker {
	return cohereReranker{client: client}
}

func (r cohereReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankMatch, error) {
	items := make([]*cohere.RerankRequestDocumentsItem, len(docs))
	for i, d := range docs {
		items[i] = &cohere.RerankRequestDocumentsItem{String: d}
	}
	resp, err := r.client.Rerank(ctx, &cohere.RerankRequest{
		Model:     stringPtr(rerankModel),
		Query:     query,
		Documents: items,
		TopN:      &topN,
	})
	if err != nil {
		return nil, err
	}
	matches := make([]RerankMatch, len(resp.Results))
	for i, res := range resp.Results {
		matches[i] = RerankMatch{Index: res.Index, RelevanceScore: res.RelevanceScore}
	}
	return matches, nil
}

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

func (i pineconeIndex) Query(ctx context.Context, vector []float32, topK uint32, filter *pinecone.MetadataFilter) ([]QueryMatch, error) {
	resp, err := i.conn.QueryByVectorValues(ctx, &pinecone.QueryByVectorValuesRequest{
		Vector: vector, TopK: topK, MetadataFilter: filter, IncludeMetadata: true,
	})
	if err != nil {
		return nil, err
	}
	matches := make([]QueryMatch, len(resp.Matches))
	for i, m := range resp.Matches {
		var metadata map[string]any
		if m.Vector != nil && m.Vector.Metadata != nil {
			metadata = m.Vector.Metadata.AsMap()
		}
		id := ""
		if m.Vector != nil {
			id = m.Vector.Id
		}
		matches[i] = QueryMatch{ID: id, Score: float64(m.Score), Metadata: metadata}
	}
	return matches, nil
}
