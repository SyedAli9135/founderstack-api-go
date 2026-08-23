//go:build integration

package documents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// --- fakes for the 3 external dependencies (BlobStore, Embedder,
// VectorIndex) — see deps.go's doc comment for why Processor depends on
// interfaces instead of the concrete S3/Cohere/Pinecone SDK clients. No
// LocalStack, Cohere, or Pinecone dependency, and no new CI secrets or
// services needed to run this file. ---

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}}
}

func (s *fakeStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	return nil
}

func (s *fakeStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, errors.New("fakeStore: object not found: " + key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}

type fakeEmbedder struct {
	err error
}

func (e *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float64, len(texts))
	for i := range out {
		out[i] = []float64{0.1, 0.2, 0.3}
	}
	return out, nil
}

type fakeVectorIndexState struct {
	mu       sync.Mutex
	upserted map[string][]string // namespace -> pinecone IDs upserted
	deleted  map[string][]string // namespace -> pinecone IDs deleted
}

type fakeVectorIndex struct {
	state *fakeVectorIndexState
	ns    string
}

func newFakeVectorIndex() *fakeVectorIndex {
	return &fakeVectorIndex{state: &fakeVectorIndexState{
		upserted: map[string][]string{},
		deleted:  map[string][]string{},
	}}
}

func (v *fakeVectorIndex) Namespace(ns string) VectorIndex {
	return &fakeVectorIndex{state: v.state, ns: ns}
}

func (v *fakeVectorIndex) Upsert(ctx context.Context, vectors []*pinecone.Vector) error {
	v.state.mu.Lock()
	defer v.state.mu.Unlock()
	for _, vec := range vectors {
		v.state.upserted[v.ns] = append(v.state.upserted[v.ns], vec.Id)
	}
	return nil
}

func (v *fakeVectorIndex) DeleteByID(ctx context.Context, ids []string) error {
	v.state.mu.Lock()
	defer v.state.mu.Unlock()
	v.state.deleted[v.ns] = append(v.state.deleted[v.ns], ids...)
	return nil
}

// --- shared test scaffolding (Postgres, org/user) ---

func testAppPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to app test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testSystemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to system test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func testOrg(t *testing.T, systemPool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()

	var orgID pgtype.UUID
	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Documents Test Org', $2) returning id",
		"org_documents_test_"+suffix, "documents-test-"+suffix,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}

	var userID pgtype.UUID
	err = systemPool.QueryRow(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'documents-test@example.com') returning id`,
		orgID, "user_documents_test_"+suffix,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})
	return orgID
}

func testUploader(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var userID pgtype.UUID
	err := systemPool.QueryRow(context.Background(),
		"select id from users where org_id = $1 limit 1", orgID,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("look up test user: %v", err)
	}
	return userID
}

func newTestUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	// RFC 4122 version 4 bits, matching how google/uuid.New() shapes its
	// output — not load-bearing for these tests, just avoids a visibly
	// malformed UUID in failure output.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return pgtype.UUID{Bytes: b, Valid: true}
}

func insertPendingDocument(t *testing.T, appPool *pgxpool.Pool, orgID, uploadedBy pgtype.UUID, filename string) pgtype.UUID {
	t.Helper()
	docID := newTestUUID(t)
	s3Path := "documents/" + orgID.String() + "/" + docID.String() + "/" + filename
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertDocument(ctx, dbgen.InsertDocumentParams{
			ID:         docID,
			OrgID:      orgID,
			Filename:   filename,
			S3Path:     s3Path,
			UploadedBy: uploadedBy,
		})
	})
	if err != nil {
		t.Fatalf("insert pending document: %v", err)
	}
	return docID
}

func getDocument(t *testing.T, appPool *pgxpool.Pool, orgID, docID pgtype.UUID) dbgen.GetDocumentRow {
	t.Helper()
	var row dbgen.GetDocumentRow
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		row, err = q.GetDocument(ctx, dbgen.GetDocumentParams{OrgID: orgID, ID: docID})
		return err
	})
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	return row
}

// --- Process ---

func TestProcessor_Process_Success(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID := testOrg(t, systemPool)
	uploadedBy := testUploader(t, systemPool, orgID)

	docID := insertPendingDocument(t, appPool, orgID, uploadedBy, "notes.txt")
	store := newFakeStore()
	s3Path := "documents/" + orgID.String() + "/" + docID.String() + "/notes.txt"
	store.objects[s3Path] = []byte("FounderStack lets a founder upload documents so their agents can reference them when they run. " +
		"This text is long enough to produce more than one chunk once split, so the test can assert real chunk counts.")

	index := newFakeVectorIndex()
	p := NewProcessor(appPool, store, &fakeEmbedder{}, index)

	if err := p.Process(context.Background(), orgID, docID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	doc := getDocument(t, appPool, orgID, docID)
	if doc.ProcessingStatus == nil || *doc.ProcessingStatus != "indexed" {
		t.Fatalf("processing_status = %v, want indexed", doc.ProcessingStatus)
	}
	if doc.TotalChunks == nil || *doc.TotalChunks < 1 {
		t.Fatalf("total_chunks = %v, want >= 1", doc.TotalChunks)
	}
	if !doc.IndexedAt.Valid {
		t.Fatal("indexed_at not set")
	}

	ns := "org_" + orgID.String()
	if len(index.state.upserted[ns]) != int(*doc.TotalChunks) {
		t.Fatalf("upserted %d vectors in namespace %q, want %d", len(index.state.upserted[ns]), ns, *doc.TotalChunks)
	}

	var chunkCount int
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		ids, err := q.ListDocumentChunkPineconeIDs(ctx, docID)
		chunkCount = len(ids)
		return err
	})
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if chunkCount != int(*doc.TotalChunks) {
		t.Fatalf("document_chunks rows = %d, want %d", chunkCount, *doc.TotalChunks)
	}
}

func TestProcessor_Process_EmbedFailureMarksDocumentFailed(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID := testOrg(t, systemPool)
	uploadedBy := testUploader(t, systemPool, orgID)

	docID := insertPendingDocument(t, appPool, orgID, uploadedBy, "notes.txt")
	store := newFakeStore()
	s3Path := "documents/" + orgID.String() + "/" + docID.String() + "/notes.txt"
	store.objects[s3Path] = []byte("some content to embed")

	embedErr := errors.New("synthetic embed failure")
	p := NewProcessor(appPool, store, &fakeEmbedder{err: embedErr}, newFakeVectorIndex())

	if err := p.Process(context.Background(), orgID, docID); err == nil {
		t.Fatal("Process() error = nil, want the embed failure to propagate")
	}

	doc := getDocument(t, appPool, orgID, docID)
	if doc.ProcessingStatus == nil || *doc.ProcessingStatus != "failed" {
		t.Fatalf("processing_status = %v, want failed", doc.ProcessingStatus)
	}
	if doc.ErrorDetail == nil || *doc.ErrorDetail == "" {
		t.Fatal("error_detail not set on failure")
	}
}

// --- Purge ---

func TestProcessor_Purge_RemovesVectorsFileAndRows(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID := testOrg(t, systemPool)
	uploadedBy := testUploader(t, systemPool, orgID)

	docID := insertPendingDocument(t, appPool, orgID, uploadedBy, "notes.txt")
	store := newFakeStore()
	s3Path := "documents/" + orgID.String() + "/" + docID.String() + "/notes.txt"
	store.objects[s3Path] = []byte("some content that will be chunked and embedded before this document is purged")

	index := newFakeVectorIndex()
	p := NewProcessor(appPool, store, &fakeEmbedder{}, index)
	if err := p.Process(context.Background(), orgID, docID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	if err := p.Purge(context.Background(), orgID, docID); err != nil {
		t.Fatalf("Purge() error = %v", err)
	}

	if _, ok := store.objects[s3Path]; ok {
		t.Fatal("s3 object still present after purge")
	}
	ns := "org_" + orgID.String()
	if len(index.state.deleted[ns]) == 0 {
		t.Fatal("no vectors were deleted from the index")
	}

	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.GetDocument(ctx, dbgen.GetDocumentParams{OrgID: orgID, ID: docID})
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetDocument after purge error = %v, want pgx.ErrNoRows", err)
	}
}

func TestProcessor_Purge_MissingDocumentIsNoop(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID := testOrg(t, systemPool)

	store := newFakeStore()
	index := newFakeVectorIndex()
	p := NewProcessor(appPool, store, &fakeEmbedder{}, index)

	if err := p.Purge(context.Background(), orgID, newTestUUID(t)); err != nil {
		t.Fatalf("Purge() on a nonexistent document error = %v, want nil (idempotent)", err)
	}
	if len(store.deleted) != 0 {
		t.Fatal("Purge on a missing document should not touch the store")
	}
}

// --- Reindex ---

func TestProcessor_Reindex_ReplacesChunks(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	orgID := testOrg(t, systemPool)
	uploadedBy := testUploader(t, systemPool, orgID)

	docID := insertPendingDocument(t, appPool, orgID, uploadedBy, "notes.txt")
	store := newFakeStore()
	s3Path := "documents/" + orgID.String() + "/" + docID.String() + "/notes.txt"
	store.objects[s3Path] = []byte("original content for the first indexing pass")

	index := newFakeVectorIndex()
	p := NewProcessor(appPool, store, &fakeEmbedder{}, index)
	if err := p.Process(context.Background(), orgID, docID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	var firstPineconeIDs []string
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		firstPineconeIDs, err = q.ListDocumentChunkPineconeIDs(ctx, docID)
		return err
	})
	if err != nil {
		t.Fatalf("list chunks after first process: %v", err)
	}

	if err := p.Reindex(context.Background(), orgID, docID); err != nil {
		t.Fatalf("Reindex() error = %v", err)
	}

	doc := getDocument(t, appPool, orgID, docID)
	if doc.ProcessingStatus == nil || *doc.ProcessingStatus != "indexed" {
		t.Fatalf("processing_status after reindex = %v, want indexed", doc.ProcessingStatus)
	}

	var secondPineconeIDs []string
	err = tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		secondPineconeIDs, err = q.ListDocumentChunkPineconeIDs(ctx, docID)
		return err
	})
	if err != nil {
		t.Fatalf("list chunks after reindex: %v", err)
	}
	if len(secondPineconeIDs) != len(firstPineconeIDs) {
		t.Fatalf("chunk count after reindex = %d, want %d (same content, same chunking)", len(secondPineconeIDs), len(firstPineconeIDs))
	}
}
