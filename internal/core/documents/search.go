package documents

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
)

// overRetrieveTopK over-retrieve, then rerank down to
// the caller's requested topK, so rerank has real candidates to work with
// beyond whatever narrow topK a founder asked for.
const overRetrieveTopK = 20

// HyDEGenerator produces a short hypothetical answer to query — Hypothetical
// Document Embeddings: embedding a plausible *answer* retrieves better than
// embedding the *question* alone, since real indexed chunks read like
// answers, not questions. Defined here (the consumer), implemented in
// internal/api/documents against the org's resolved BYOK ChatClient — this
// package deliberately doesn't import internal/core/llm itself.
type HyDEGenerator interface {
	GenerateHypotheticalAnswer(ctx context.Context, query string) (string, error)
}

// SearchChunk is one ranked result — RelevanceScore is the reranker's score
// when a Reranker was used, otherwise Pinecone's own cosine-similarity score.
type SearchChunk struct {
	DocID          string
	ChunkIndex     int
	Text           string
	RelevanceScore float64
}

// Searcher runs the query-embed -> Pinecone-query -> rerank pipeline
// (workflow 12) — the search-side counterpart to Processor's ingestion
// pipeline, same interface-segregation reasoning (fakeable in tests, no
// live Cohere/Pinecone needed).
type Searcher struct {
	embedder Embedder
	index    VectorIndex
	reranker Reranker // nil is valid: falls back to Pinecone's own ordering
}

func NewSearcher(embedder Embedder, index VectorIndex, reranker Reranker) *Searcher {
	return &Searcher{embedder: embedder, index: index, reranker: reranker}
}

// Search embeds query (averaged with a HyDE hypothetical answer's own
// embedding when hyde is non-nil and succeeds), queries orgID's Pinecone
// namespace under filter, and returns at most topK chunks. hyde is a
// per-call parameter, not a Searcher field: whether HyDE runs at all
// depends on the requesting org's own BYOK configuration, resolved fresh
// per request — Searcher itself is a shared, org-agnostic singleton.
// A HyDE failure never fails the search — it degrades to the query
// embedding alone, same "notification channel degrades to logged no-op"
// philosophy as workflow 10's internal/core/notify.
func (s *Searcher) Search(ctx context.Context, orgID pgtype.UUID, query string, hyde HyDEGenerator, filter *pinecone.MetadataFilter, topK int) ([]SearchChunk, error) {
	queryVec, err := s.embedder.EmbedOne(ctx, query, EmbedModeQuery)
	if err != nil {
		return nil, fmt.Errorf("documents: embed query: %w", err)
	}

	if hyde != nil {
		if hypothetical, err := hyde.GenerateHypotheticalAnswer(ctx, query); err != nil {
			slog.Warn("documents: HyDE generation failed, searching with the raw query embedding only", "err", err)
		} else if hypothetical != "" {
			hydeVec, err := s.embedder.EmbedOne(ctx, hypothetical, EmbedModeDocument)
			if err != nil {
				slog.Warn("documents: HyDE embedding failed, searching with the raw query embedding only", "err", err)
			} else {
				queryVec = averageVectors(queryVec, hydeVec)
			}
		}
	}

	values := make([]float32, len(queryVec))
	for i, v := range queryVec {
		values[i] = float32(v)
	}

	idx := s.index.Namespace(namespaceForOrg(orgID))
	matches, err := idx.Query(ctx, values, overRetrieveTopK, filter)
	if err != nil {
		return nil, fmt.Errorf("documents: query pinecone: %w", err)
	}
	if len(matches) == 0 {
		return nil, nil
	}

	if s.reranker != nil {
		texts := make([]string, len(matches))
		for i, m := range matches {
			texts[i], _ = m.Metadata["text"].(string)
		}
		n := min(topK, len(matches))
		reranked, err := s.reranker.Rerank(ctx, query, texts, n)
		if err != nil {
			slog.Warn("documents: rerank failed, falling back to Pinecone's own ordering", "err", err)
		} else {
			return chunksFromRerank(matches, reranked), nil
		}
	}

	if topK < len(matches) {
		matches = matches[:topK]
	}
	return chunksFromMatches(matches), nil
}

func averageVectors(a, b []float64) []float64 {
	if len(a) != len(b) {
		// Different embedding dimensionality shouldn't happen (same model
		// embeds both), but average is meaningless if it does — fail safe
		// to the query vector alone rather than panic on an index mismatch.
		return a
	}
	out := make([]float64, len(a))
	for i := range a {
		out[i] = (a[i] + b[i]) / 2
	}
	return out
}

func chunksFromMatches(matches []QueryMatch) []SearchChunk {
	out := make([]SearchChunk, len(matches))
	for i, m := range matches {
		out[i] = chunkFromMatch(m, m.Score)
	}
	return out
}

// chunksFromRerank reorders matches by the reranker's own ranking (already
// sorted best-first by RelevanceScore) rather than trusting index order.
func chunksFromRerank(matches []QueryMatch, reranked []RerankMatch) []SearchChunk {
	sort.SliceStable(reranked, func(i, j int) bool { return reranked[i].RelevanceScore > reranked[j].RelevanceScore })
	out := make([]SearchChunk, 0, len(reranked))
	for _, r := range reranked {
		if r.Index < 0 || r.Index >= len(matches) {
			continue // defensive: a malformed index shouldn't panic a search request
		}
		out = append(out, chunkFromMatch(matches[r.Index], r.RelevanceScore))
	}
	return out
}

func chunkFromMatch(m QueryMatch, score float64) SearchChunk {
	docID, _ := m.Metadata["doc_id"].(string)
	chunkIndex := 0
	if v, ok := m.Metadata["chunk_index"].(float64); ok { // protobuf/JSON numbers decode as float64
		chunkIndex = int(v)
	}
	text, _ := m.Metadata["text"].(string)
	return SearchChunk{DocID: docID, ChunkIndex: chunkIndex, Text: text, RelevanceScore: score}
}
