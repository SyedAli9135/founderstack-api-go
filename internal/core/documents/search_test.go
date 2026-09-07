package documents

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
)

type stubEmbedder struct {
	vectors map[string][]float64 // text -> embedding, so a test can tell which text produced which vector
}

func (e stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	panic("not used by Searcher")
}

func (e stubEmbedder) EmbedOne(ctx context.Context, text string, mode EmbedMode) ([]float64, error) {
	if v, ok := e.vectors[text]; ok {
		return v, nil
	}
	return []float64{0, 0}, nil
}

type spyVectorIndex struct {
	matches      []QueryMatch
	lastVector   []float32
	namespaceGot string
}

func (v *spyVectorIndex) Namespace(ns string) VectorIndex { v.namespaceGot = ns; return v }
func (v *spyVectorIndex) Upsert(ctx context.Context, vectors []*pinecone.Vector) error {
	panic("not used by Searcher")
}
func (v *spyVectorIndex) DeleteByID(ctx context.Context, ids []string) error {
	panic("not used by Searcher")
}
func (v *spyVectorIndex) Query(ctx context.Context, vector []float32, topK uint32, filter *pinecone.MetadataFilter) ([]QueryMatch, error) {
	v.lastVector = vector
	return v.matches, nil
}

type stubHyDE struct {
	answer string
	err    error
}

func (h stubHyDE) GenerateHypotheticalAnswer(ctx context.Context, query string) (string, error) {
	return h.answer, h.err
}

func testOrgID() pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{1, 2, 3}, Valid: true}
}

func TestSearcher_Search_NoHyDEUsesRawQueryVectorOnly(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{"my query": {1, 2, 3}}}
	index := &spyVectorIndex{matches: []QueryMatch{{ID: "d-0", Score: 0.9, Metadata: map[string]any{"doc_id": "d", "chunk_index": float64(0), "text": "hi"}}}}
	s := NewSearcher(embedder, index, nil)

	chunks, err := s.Search(context.Background(), testOrgID(), "my query", nil, nil, 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(chunks) != 1 || chunks[0].DocID != "d" || chunks[0].Text != "hi" {
		t.Fatalf("chunks = %+v", chunks)
	}
	want := []float32{1, 2, 3}
	if len(index.lastVector) != len(want) {
		t.Fatalf("Query received vector %v, want %v (the raw query embedding, unaveraged)", index.lastVector, want)
	}
	for i := range want {
		if index.lastVector[i] != want[i] {
			t.Fatalf("Query received vector %v, want %v", index.lastVector, want)
		}
	}
}

func TestSearcher_Search_HyDEAveragesQueryAndHypotheticalVectors(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{
		"my query":          {0, 0, 0},
		"hypothetical text": {2, 4, 6},
	}}
	index := &spyVectorIndex{}
	s := NewSearcher(embedder, index, nil)

	_, err := s.Search(context.Background(), testOrgID(), "my query", stubHyDE{answer: "hypothetical text"}, nil, 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []float32{1, 2, 3} // average of (0,0,0) and (2,4,6)
	if len(index.lastVector) != 3 || index.lastVector[0] != want[0] || index.lastVector[1] != want[1] || index.lastVector[2] != want[2] {
		t.Fatalf("Query received vector %v, want the averaged vector %v", index.lastVector, want)
	}
}

func TestSearcher_Search_HyDEFailureDegradesToQueryVectorAlone(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{"my query": {5, 5, 5}}}
	index := &spyVectorIndex{}
	s := NewSearcher(embedder, index, nil)

	_, err := s.Search(context.Background(), testOrgID(), "my query", stubHyDE{err: errors.New("boom")}, nil, 5)
	if err != nil {
		t.Fatalf("Search() error = %v, want nil (a HyDE failure must not fail the search)", err)
	}
	want := []float32{5, 5, 5}
	if len(index.lastVector) != 3 || index.lastVector[0] != want[0] {
		t.Fatalf("Query received vector %v, want the raw query vector %v (HyDE failed, should degrade)", index.lastVector, want)
	}
}

func TestSearcher_Search_NamespaceScopedToOrg(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{}}
	index := &spyVectorIndex{}
	s := NewSearcher(embedder, index, nil)
	orgID := testOrgID()

	if _, err := s.Search(context.Background(), orgID, "q", nil, nil, 5); err != nil {
		t.Fatal(err)
	}
	want := namespaceForOrg(orgID)
	if index.namespaceGot != want {
		t.Fatalf("Namespace() called with %q, want %q", index.namespaceGot, want)
	}
}

func TestSearcher_Search_NoMatchesReturnsEmptyNotError(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{}}
	index := &spyVectorIndex{matches: nil}
	s := NewSearcher(embedder, index, nil)

	chunks, err := s.Search(context.Background(), testOrgID(), "q", nil, nil, 5)
	if err != nil {
		t.Fatalf("Search() error = %v, want nil", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("chunks = %+v, want empty", chunks)
	}
}

type stubReranker struct {
	matches []RerankMatch
	err     error
	gotDocs []string
}

func (r *stubReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankMatch, error) {
	r.gotDocs = docs
	if r.err != nil {
		return nil, r.err
	}
	return r.matches, nil
}

func TestSearcher_Search_RerankerReordersResults(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{}}
	index := &spyVectorIndex{matches: []QueryMatch{
		{ID: "a-0", Score: 0.5, Metadata: map[string]any{"doc_id": "a", "chunk_index": float64(0), "text": "low relevance"}},
		{ID: "b-0", Score: 0.4, Metadata: map[string]any{"doc_id": "b", "chunk_index": float64(0), "text": "high relevance"}},
	}}
	// Reranker flips Pinecone's own order: index 1 (b, "high relevance") should now rank first.
	reranker := &stubReranker{matches: []RerankMatch{{Index: 1, RelevanceScore: 0.99}, {Index: 0, RelevanceScore: 0.1}}}
	s := NewSearcher(embedder, index, reranker)

	chunks, err := s.Search(context.Background(), testOrgID(), "q", nil, nil, 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(chunks) != 2 || chunks[0].DocID != "b" || chunks[0].RelevanceScore != 0.99 {
		t.Fatalf("chunks = %+v, want b first with the reranker's own score", chunks)
	}
	if len(reranker.gotDocs) != 2 || reranker.gotDocs[0] != "low relevance" || reranker.gotDocs[1] != "high relevance" {
		t.Fatalf("Rerank() called with docs = %v, want Pinecone's own candidate texts in order", reranker.gotDocs)
	}
}

func TestSearcher_Search_RerankFailureFallsBackToPineconeOrdering(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{}}
	index := &spyVectorIndex{matches: []QueryMatch{
		{ID: "a-0", Score: 0.9, Metadata: map[string]any{"doc_id": "a", "chunk_index": float64(0), "text": "first"}},
		{ID: "b-0", Score: 0.1, Metadata: map[string]any{"doc_id": "b", "chunk_index": float64(0), "text": "second"}},
	}}
	reranker := &stubReranker{err: errors.New("cohere rerank outage")}
	s := NewSearcher(embedder, index, reranker)

	chunks, err := s.Search(context.Background(), testOrgID(), "q", nil, nil, 5)
	if err != nil {
		t.Fatalf("Search() error = %v, want nil (a rerank failure must not fail the search)", err)
	}
	if len(chunks) != 2 || chunks[0].DocID != "a" || chunks[0].RelevanceScore != 0.9 {
		t.Fatalf("chunks = %+v, want Pinecone's own order/score preserved", chunks)
	}
}

func TestSearcher_Search_TopKTruncatesWithoutReranker(t *testing.T) {
	embedder := stubEmbedder{vectors: map[string][]float64{}}
	index := &spyVectorIndex{matches: []QueryMatch{
		{ID: "a-0", Score: 0.9, Metadata: map[string]any{"doc_id": "a", "chunk_index": float64(0), "text": "1"}},
		{ID: "b-0", Score: 0.8, Metadata: map[string]any{"doc_id": "b", "chunk_index": float64(0), "text": "2"}},
		{ID: "c-0", Score: 0.7, Metadata: map[string]any{"doc_id": "c", "chunk_index": float64(0), "text": "3"}},
	}}
	s := NewSearcher(embedder, index, nil)

	chunks, err := s.Search(context.Background(), testOrgID(), "q", nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v, want exactly 2 (topK)", chunks)
	}
}

func TestAverageVectors(t *testing.T) {
	got := averageVectors([]float64{1, 2, 3}, []float64{3, 4, 5})
	want := []float64{2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("averageVectors() = %v, want %v", got, want)
		}
	}
}

func TestAverageVectors_MismatchedLengthFallsBackToFirst(t *testing.T) {
	a := []float64{1, 2, 3}
	got := averageVectors(a, []float64{1, 2})
	if len(got) != len(a) || got[0] != a[0] {
		t.Fatalf("averageVectors() = %v, want the first vector unchanged on a length mismatch", got)
	}
}
