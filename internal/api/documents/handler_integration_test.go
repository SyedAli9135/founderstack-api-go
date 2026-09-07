//go:build integration

package documents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	coredocs "github.com/founderstack/api/internal/core/documents"
	"github.com/founderstack/api/internal/core/llm"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// --- fakes for the handler's 2 external dependencies, matching
// internal/core/documents/processor_integration_test.go's reasoning: no
// LocalStack/Cohere/Pinecone dependency needed to exercise the real HTTP
// lifecycle. Not shared with that file (different package, and Go test
// doubles are conventionally package-local, not exported test infra) —
// same light duplication internal/api/integrations/handler_integration_test.go
// already accepts for its own fakeOAuthProvider/fakeKeyProvider. ---

type fakeBlobStore struct {
	objects map[string][]byte
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{objects: map[string][]byte{}}
}

func (s *fakeBlobStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = data
	return nil
}

func (s *fakeBlobStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.objects[key])), nil
}

func (s *fakeBlobStore) Delete(ctx context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range out {
		out[i] = []float64{0.1, 0.2, 0.3}
	}
	return out, nil
}

func (fakeEmbedder) EmbedOne(ctx context.Context, text string, mode coredocs.EmbedMode) ([]float64, error) {
	return []float64{0.1, 0.2, 0.3}, nil
}

// fakeVectorIndex.Query genuinely respects a doc_id $in filter (decoded
// straight out of the real *pinecone.MetadataFilter/structpb shape) rather
// than ignoring it — otherwise a search-ACL test could pass for the wrong
// reason (the filter never even reaching Pinecone, not the filter working).
type fakeVectorIndex struct {
	results []coredocs.QueryMatch
}

func (v fakeVectorIndex) Namespace(ns string) coredocs.VectorIndex { return v }
func (v fakeVectorIndex) Upsert(ctx context.Context, vectors []*pinecone.Vector) error {
	return nil
}
func (v fakeVectorIndex) DeleteByID(ctx context.Context, ids []string) error { return nil }
func (v fakeVectorIndex) Query(ctx context.Context, vector []float32, topK uint32, filter *pinecone.MetadataFilter) ([]coredocs.QueryMatch, error) {
	allowed := allowedDocIDsFromFilter(filter)
	if allowed == nil {
		return v.results, nil
	}
	out := make([]coredocs.QueryMatch, 0, len(v.results))
	for _, m := range v.results {
		if docID, _ := m.Metadata["doc_id"].(string); allowed[docID] {
			out = append(out, m)
		}
	}
	return out, nil
}

// allowedDocIDsFromFilter decodes {"doc_id": {"$in": [...]}} back out of
// the real structpb shape internal/api/documents.Search builds. Returns
// nil (not filtered) if the filter isn't in that exact shape.
func allowedDocIDsFromFilter(filter *pinecone.MetadataFilter) map[string]bool {
	if filter == nil {
		return nil
	}
	docIDField, ok := filter.Fields["doc_id"]
	if !ok {
		return nil
	}
	inField, ok := docIDField.GetStructValue().Fields["$in"]
	if !ok {
		return nil
	}
	values := inField.GetListValue().GetValues()
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v.GetStringValue()] = true
	}
	return out
}

type fakeHyDE struct {
	answer string
	err    error
}

func (f fakeHyDE) GenerateHypotheticalAnswer(ctx context.Context, query string) (string, error) {
	return f.answer, f.err
}

// fakeReranker returns docs in the order given, descending scores — the
// point of these tests is verifying the wiring (ACL, caching, hydration),
// not Cohere's actual ranking quality.
type fakeReranker struct{}

func (fakeReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]coredocs.RerankMatch, error) {
	n := min(topN, len(docs))
	out := make([]coredocs.RerankMatch, n)
	for i := 0; i < n; i++ {
		out[i] = coredocs.RerankMatch{Index: i, RelevanceScore: float64(n-i) / float64(n)}
	}
	return out, nil
}

type fakeChatClient struct {
	content string
}

func (f fakeChatClient) Send(ctx context.Context, systemPrompt string, messages []llm.Message, tools []llm.ToolSchema) (llm.ChatResponse, error) {
	return llm.ChatResponse{Content: f.content, StopReason: llm.StopReasonEndTurn}, nil
}

// fakeChatClientResolver matches chatClientResolver's signature — err
// simulates "no active BYOK key" (llm.ResolveChatClient's real failure
// mode), which Handler.resolveHyDE must degrade from, not fail on.
func fakeChatClientResolver(client llm.ChatClient, err error) chatClientResolver {
	return func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error) {
		return client, err
	}
}

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable at localhost:6379 (run make docker-up): %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// --- shared test scaffolding ---

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

func testOrgAndUser(t *testing.T, systemPool *pgxpool.Pool) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	suffix := randSuffix(t)
	clerkUserID = "user_documents_test_" + suffix
	ctx := context.Background()

	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Documents Test Org', $2) returning id",
		"org_documents_test_"+suffix, "documents-test-"+suffix,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	_, err = systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email) values ($1, $2, 'documents-test@example.com')`,
		orgID, clerkUserID,
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})
	return orgID, clerkUserID
}

// testOrgAndUserWithRole is testOrgAndUser plus an explicit role — search's
// ACL check (Handler.Search: user.Role == "owner" || user.Role == "admin")
// needs a non-default role to exercise the "member can't see owner_only"
// and "owner can" cases in the same suite.
func testOrgAndUserWithRole(t *testing.T, systemPool *pgxpool.Pool, role string) (orgID pgtype.UUID, clerkUserID string) {
	t.Helper()
	orgID, clerkUserID = testOrgAndUser(t, systemPool)
	if _, err := systemPool.Exec(context.Background(),
		"update users set role = $1 where clerk_user_id = $2", role, clerkUserID,
	); err != nil {
		t.Fatalf("set user role: %v", err)
	}
	return orgID, clerkUserID
}

// insertIndexedDocument seeds a real 'indexed' documents row directly —
// Search reads Postgres for ACL/category filtering and hydration, not
// Pinecone, so a document doesn't need a real ingestion pass to be
// searchable in these tests (the fakeVectorIndex results are supplied
// separately, keyed by this returned doc ID).
func insertIndexedDocument(t *testing.T, appPool *pgxpool.Pool, orgID pgtype.UUID, filename, category, visibility string) pgtype.UUID {
	t.Helper()
	docID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	err := tenant.WithTx(context.Background(), appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		if err := q.InsertDocument(ctx, dbgen.InsertDocumentParams{
			ID: docID, OrgID: orgID, Filename: filename, S3Path: "documents/" + orgID.String() + "/" + docID.String() + "/" + filename,
			Category: &category, UploadedBy: pgtype.UUID{}, Visibility: visibility,
		}); err != nil {
			return err
		}
		totalChunks := int32(1)
		return q.MarkDocumentIndexed(ctx, dbgen.MarkDocumentIndexedParams{OrgID: orgID, ID: docID, TotalChunks: &totalChunks})
	})
	if err != nil {
		t.Fatalf("insert indexed document: %v", err)
	}
	return docID
}

// fakeMatch builds a coredocs.QueryMatch the way a real Pinecone vector for
// docID/chunkIndex/text would decode to (see processor.go's own metadata
// shape) — chunk_index intentionally float64, matching how JSON/protobuf
// numbers actually decode (chunkFromMatch's own doc comment).
func fakeMatch(docID pgtype.UUID, chunkIndex int, text string, score float64) coredocs.QueryMatch {
	return coredocs.QueryMatch{
		ID:    docID.String() + "-" + fmt.Sprint(chunkIndex),
		Score: score,
		Metadata: map[string]any{
			"doc_id": docID.String(), "chunk_index": float64(chunkIndex), "text": text,
		},
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, store coredocs.BlobStore, processor *coredocs.Processor) *gin.Engine {
	t.Helper()
	return testSearchRouter(t, systemPool, appPool, cfg, store, processor, nil, nil, fakeChatClientResolver(nil, errNoKeyConfigured))
}

// testSearchRouter is testRouter with the workflow-12 deps a search test
// actually needs (searcher/redis/HyDE resolver) — kept separate so the
// existing upload/list/get/delete/reindex tests above don't need to know
// or care about any of this.
func testSearchRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, store coredocs.BlobStore, processor *coredocs.Processor, searcher *coredocs.Searcher, rdb *redis.Client, resolver chatClientResolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	h := NewHandlerWithResolver(appPool, store, processor, searcher, rdb, []byte("test-encryption-key-32-bytes!!!"), resolver)
	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	h.Register(authed)

	return r
}

var errNoKeyConfigured = fmt.Errorf("no active key configured")

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, path string, body io.Reader, contentType string) *http.Request {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func multipartUploadBody(t *testing.T, filename, category string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if category != "" {
		if err := w.WriteField("category", category); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type documentDetailView struct {
	ProcessingStatus string     `json:"processing_status"`
	IndexedAt        *time.Time `json:"indexed_at,omitempty"`
}

func getDocumentDetail(t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID, docID string) (documentDetailView, int) {
	t.Helper()
	req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/documents/"+docID, nil, "")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return documentDetailView{}, rec.Code
	}
	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var doc documentDetailView
	if err := json.Unmarshal(env.Data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, rec.Code
}

// waitForFreshIndexed polls GET /api/v1/documents/{id} until it reaches
// 'indexed' with an indexed_at different from staleIndexedAt, or 'failed'
// — Upload/Reindex both kick off real background goroutines
// (context.Background(), same as production), so their effects are only
// visible after that goroutine runs, not synchronously when the HTTP call
// returns. staleIndexedAt matters specifically for reindex: without it, a
// poll that lands before the reindex goroutine has even reset the status
// to 'pending' would see the *previous* run's leftover 'indexed' value and
// return immediately, never actually observing the new run — pass nil for
// a first-time upload, where there is no previous run to be fooled by.
func waitForFreshIndexed(t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID, docID string, staleIndexedAt *time.Time) documentDetailView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last documentDetailView
	for time.Now().Before(deadline) {
		doc, code := getDocumentDetail(t, router, cfg, clerkUserID, docID)
		if code == http.StatusOK {
			last = doc
			if doc.ProcessingStatus == "failed" {
				return doc
			}
			if doc.ProcessingStatus == "indexed" && doc.IndexedAt != nil &&
				(staleIndexedAt == nil || !doc.IndexedAt.Equal(*staleIndexedAt)) {
				return doc
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("document %s did not reach a fresh terminal status within the deadline; last = %+v", docID, last)
	return last
}

func TestDocumentsHandler_FullLifecycle(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool)

	store := newFakeBlobStore()
	processor := coredocs.NewProcessor(appPool, store, fakeEmbedder{}, fakeVectorIndex{})
	router := testRouter(t, systemPool, appPool, cfg, store, processor)

	t.Run("unauthenticated upload is rejected", func(t *testing.T) {
		body, ct := multipartUploadBody(t, "notes.txt", "", []byte("hello"))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/documents/upload", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("unsupported file type is rejected", func(t *testing.T) {
		body, ct := multipartUploadBody(t, "malware.exe", "", []byte("hello"))
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/documents/upload", body, ct)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing file is rejected", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/documents/upload", nil, "")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get unknown document is 404", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/documents/00000000-0000-0000-0000-000000000000", nil, "")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get invalid document id is 400", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/documents/not-a-uuid", nil, "")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	var docID string
	t.Run("upload accepts a valid text file", func(t *testing.T) {
		content := []byte("FounderStack lets a founder upload documents so their agents can reference them at run time.")
		body, ct := multipartUploadBody(t, "notes.txt", "legal", content)
		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/documents/upload", body, ct)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var got struct {
			DocID  string `json:"doc_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.DocID == "" || got.Status != "processing" {
			t.Fatalf("unexpected upload response: %+v", got)
		}
		docID = got.DocID
	})

	t.Run("background processing reaches indexed", func(t *testing.T) {
		doc := waitForFreshIndexed(t, router, cfg, clerkUserID, docID, nil)
		if doc.ProcessingStatus != "indexed" {
			t.Fatalf("processing_status = %q, want indexed", doc.ProcessingStatus)
		}
	})

	t.Run("list includes the uploaded document", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/documents", nil, "")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var env apiEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var docs []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(env.Data, &docs); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range docs {
			if d.ID == docID {
				found = true
			}
		}
		if !found {
			t.Fatalf("uploaded document %s not present in list response", docID)
		}
	})

	t.Run("reindex re-triggers processing", func(t *testing.T) {
		before, code := getDocumentDetail(t, router, cfg, clerkUserID, docID)
		if code != http.StatusOK {
			t.Fatalf("get document before reindex: status = %d", code)
		}

		req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/documents/"+docID+"/reindex", nil, "")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
		}
		doc := waitForFreshIndexed(t, router, cfg, clerkUserID, docID, before.IndexedAt)
		if doc.ProcessingStatus != "indexed" {
			t.Fatalf("processing_status after reindex = %q, want indexed", doc.ProcessingStatus)
		}
	})

	t.Run("delete removes the document from list immediately and purges it in the background", func(t *testing.T) {
		req := authedRequest(t, cfg, clerkUserID, http.MethodDelete, "/api/v1/documents/"+docID, nil, "")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}

		listReq := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/documents", nil, "")
		listRec := httptest.NewRecorder()
		router.ServeHTTP(listRec, listReq)
		var env apiEnvelope
		if err := json.Unmarshal(listRec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var docs []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(env.Data, &docs); err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if d.ID == docID {
				t.Fatal("soft-deleted document still appears in the list")
			}
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			getReq := authedRequest(t, cfg, clerkUserID, http.MethodGet, "/api/v1/documents/"+docID, nil, "")
			getRec := httptest.NewRecorder()
			router.ServeHTTP(getRec, getReq)
			if getRec.Code == http.StatusNotFound {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("document was not fully purged (hard-deleted) within the deadline")
	})
}

func doSearch(t *testing.T, router *gin.Engine, cfg *config.Config, clerkUserID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/documents/search", bytes.NewReader(payload), "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (body: %s)", err, rec.Body.String())
	}
	var data map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
	}
	return rec.Code, data
}

func TestDocumentsHandler_Search_ReturnsRerankedHydratedResults(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")

	doc1 := insertIndexedDocument(t, appPool, orgID, "handbook.pdf", "hr", "all_members")
	doc2 := insertIndexedDocument(t, appPool, orgID, "runbook.pdf", "technical", "all_members")

	index := fakeVectorIndex{results: []coredocs.QueryMatch{
		fakeMatch(doc1, 0, "vacation policy details", 0.5),
		fakeMatch(doc2, 2, "deploy runbook steps", 0.9),
	}}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(fakeChatClient{content: "a hypothetical answer"}, nil))

	code, data := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "how do I deploy?"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if data["from_cache"] != false {
		t.Errorf("from_cache = %v, want false (first request)", data["from_cache"])
	}
	results, ok := data["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %+v, want 2 items", data["results"])
	}
	// fakeReranker preserves Pinecone's own order (doc1 then doc2) but
	// assigns descending scores by position — real reranking would likely
	// reorder these; this test verifies the wiring (rerank was called,
	// its scores made it through), not real relevance judgment.
	first := results[0].(map[string]any)
	if first["doc_filename"] != "handbook.pdf" || first["category"] != "hr" || first["chunk_index"] != float64(0) {
		t.Errorf("results[0] = %+v", first)
	}
	if first["content"] != "vacation policy details" {
		t.Errorf("results[0].content = %v", first["content"])
	}
}

func TestDocumentsHandler_Search_MemberCannotSeeOwnerOnlyDoc(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")

	visibleDoc := insertIndexedDocument(t, appPool, orgID, "public.pdf", "general", "all_members")
	secretDoc := insertIndexedDocument(t, appPool, orgID, "salaries.pdf", "hr", "owner_only")

	// Pinecone (faked here) has no idea about ACLs — it returns both.
	// Enforcement has to happen via the doc_id $in filter Handler.Search
	// builds from ListSearchableDocumentIDs, which fakeVectorIndex.Query
	// genuinely applies (see allowedDocIDsFromFilter).
	index := fakeVectorIndex{results: []coredocs.QueryMatch{
		fakeMatch(visibleDoc, 0, "public info", 0.8),
		fakeMatch(secretDoc, 0, "someone's salary", 0.95),
	}}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

	code, data := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "salary info"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	results := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly 1 (owner_only doc must be excluded for a member)", results)
	}
	if results[0].(map[string]any)["doc_filename"] != "public.pdf" {
		t.Errorf("the visible doc's chunk should be the only result, got %+v", results[0])
	}
}

func TestDocumentsHandler_Search_OwnerCanSeeOwnerOnlyDoc(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "owner")

	secretDoc := insertIndexedDocument(t, appPool, orgID, "salaries.pdf", "hr", "owner_only")
	index := fakeVectorIndex{results: []coredocs.QueryMatch{fakeMatch(secretDoc, 0, "someone's salary", 0.95)}}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

	code, data := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "salary info"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	results := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly 1 (owner can see owner_only docs)", results)
	}
}

func TestDocumentsHandler_Search_NoResultsWhenNothingAllowed(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")

	// Only an owner_only doc exists — a member's allowed-doc set is empty,
	// so Handler.Search must short-circuit before ever calling Searcher.
	insertIndexedDocument(t, appPool, orgID, "salaries.pdf", "hr", "owner_only")
	// A Searcher with a vector index that panics if queried would prove
	// the short-circuit; returning an error is a simpler, equally clear signal.
	searcher := coredocs.NewSearcher(fakeEmbedder{}, panicIfQueriedIndex{}, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

	code, data := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "salary info"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	results, _ := data["results"].([]any)
	if len(results) != 0 {
		t.Fatalf("results = %+v, want empty", results)
	}

	// An ACL-empty search is still audited — a compliance-minded founder
	// cares about a zero-result attempt at an owner_only doc at least as
	// much as a successful search. Real gap: this path used to return
	// before ever reaching the audit-log insert.
	var count int
	if err := systemPool.QueryRow(context.Background(),
		"select count(*) from audit_logs where org_id = $1 and action = 'rag.search'", orgID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit_logs rows for this ACL-empty search = %d, want 1", count)
	}
}

// panicIfQueriedIndex proves TestDocumentsHandler_Search_NoResultsWhenNothingAllowed's
// short-circuit is real, not just coincidentally empty — Namespace/Query
// should never be reached when the allowed-doc-id set is empty.
type panicIfQueriedIndex struct{ coredocs.VectorIndex }

func (panicIfQueriedIndex) Namespace(ns string) coredocs.VectorIndex {
	panic("Searcher.Search should not have been called: no documents were allowed for this user")
}

func TestDocumentsHandler_Search_CategoryFilter(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")

	hrDoc := insertIndexedDocument(t, appPool, orgID, "handbook.pdf", "hr", "all_members")
	techDoc := insertIndexedDocument(t, appPool, orgID, "runbook.pdf", "technical", "all_members")

	index := fakeVectorIndex{results: []coredocs.QueryMatch{
		fakeMatch(hrDoc, 0, "vacation policy", 0.7),
		fakeMatch(techDoc, 0, "deploy steps", 0.7),
	}}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

	category := "hr"
	code, data := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "policy", "category": category})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	results := data["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["category"] != "hr" {
		t.Fatalf("results = %+v, want exactly 1 hr-category result", results)
	}
}

func TestDocumentsHandler_Search_HyDEFailureDoesNotBreakSearch(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")

	doc := insertIndexedDocument(t, appPool, orgID, "notes.pdf", "general", "all_members")
	index := fakeVectorIndex{results: []coredocs.QueryMatch{fakeMatch(doc, 0, "some notes", 0.6)}}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	// The resolver itself succeeds (org "has a provider"), but the
	// resolved ChatClient's Send call fails — Searcher.Search must
	// swallow this and still return results from the raw query embedding.
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil,
		fakeChatClientResolver(failingChatClient{}, nil))

	code, data := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "find my notes"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a HyDE failure must not fail the search)", code)
	}
	results := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %+v, want 1", results)
	}
}

type failingChatClient struct{}

func (failingChatClient) Send(ctx context.Context, systemPrompt string, messages []llm.Message, tools []llm.ToolSchema) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, fmt.Errorf("simulated provider outage")
}

func TestDocumentsHandler_Search_EmptyQueryRejected(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	_, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, nil, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

	code, _ := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": "   "})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestDocumentsHandler_Search_AuditLogNoContentStored(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	orgID, clerkUserID := testOrgAndUserWithRole(t, systemPool, "member")

	doc := insertIndexedDocument(t, appPool, orgID, "notes.pdf", "general", "all_members")
	index := fakeVectorIndex{results: []coredocs.QueryMatch{fakeMatch(doc, 0, "some notes", 0.6)}}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, nil, fakeChatClientResolver(nil, errNoKeyConfigured))

	secretQuery := "super secret query text that must never be logged"
	code, _ := doSearch(t, router, cfg, clerkUserID, map[string]any{"query": secretQuery})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	var action string
	var metadata []byte
	err := systemPool.QueryRow(context.Background(),
		"select action, metadata_info from audit_logs where org_id = $1 and action = 'rag.search'", orgID,
	).Scan(&action, &metadata)
	if err != nil {
		t.Fatalf("expected an audit_logs row for rag.search: %v", err)
	}
	if bytes.Contains(metadata, []byte(secretQuery)) {
		t.Fatalf("audit_logs.metadata_info contains the raw query text, want no content stored: %s", metadata)
	}
}

func TestDocumentsHandler_Search_CachesAndIsolatesByACLScope(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	rdb := newTestRedis(t)
	orgID, ownerClerkID := testOrgAndUserWithRole(t, systemPool, "owner")

	secretDoc := insertIndexedDocument(t, appPool, orgID, "salaries.pdf", "hr", "owner_only")
	callCount := 0
	index := &countingVectorIndex{fakeVectorIndex: fakeVectorIndex{results: []coredocs.QueryMatch{fakeMatch(secretDoc, 0, "salary data", 0.9)}}, calls: &callCount}
	searcher := coredocs.NewSearcher(fakeEmbedder{}, index, fakeReranker{})
	router := testSearchRouter(t, systemPool, appPool, cfg, newFakeBlobStore(), nil, searcher, rdb, fakeChatClientResolver(nil, errNoKeyConfigured))
	t.Cleanup(func() { rdb.FlushAll(context.Background()) })

	code, data := doSearch(t, router, cfg, ownerClerkID, map[string]any{"query": "salary lookup"})
	if code != http.StatusOK || data["from_cache"] != false {
		t.Fatalf("first request: code=%d data=%+v, want 200/from_cache=false", code, data)
	}
	if callCount != 1 {
		t.Fatalf("Pinecone query calls = %d, want 1", callCount)
	}

	code, data = doSearch(t, router, cfg, ownerClerkID, map[string]any{"query": "salary lookup"})
	if code != http.StatusOK || data["from_cache"] != true {
		t.Fatalf("second identical request: code=%d data=%+v, want 200/from_cache=true", code, data)
	}
	if callCount != 1 {
		t.Fatalf("Pinecone query calls after cache hit = %d, want still 1 (should not re-query)", callCount)
	}
	results := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("cached results = %+v, want the 1 owner_only result", results)
	}

	// A cache hit is still audited — same real gap as the ACL-empty path
	// (TestDocumentsHandler_Search_NoResultsWhenNothingAllowed): 2 distinct
	// searches so far (miss, then hit), both must be logged.
	var searchLogCount int
	if err := systemPool.QueryRow(context.Background(),
		"select count(*) from audit_logs where org_id = $1 and action = 'rag.search'", orgID,
	).Scan(&searchLogCount); err != nil {
		t.Fatal(err)
	}
	if searchLogCount != 2 {
		t.Fatalf("audit_logs rows for rag.search = %d, want 2 (the cache miss and the cache hit)", searchLogCount)
	}

	// A member issuing the identical query+category must NOT get the
	// owner's cached (owner_only-inclusive) result — the real bug this
	// project's own cache-key design caught before shipping (see
	// WORKFLOW_PLAN_GO.md's Workflow 12 scope note).
	_, memberClerkID := testOrgAndUserAsMemberOf(t, systemPool, orgID)
	code, data = doSearch(t, router, cfg, memberClerkID, map[string]any{"query": "salary lookup"})
	if code != http.StatusOK {
		t.Fatalf("member request: status = %d, want 200", code)
	}
	if data["from_cache"] == true {
		t.Fatal("member's search hit the owner's cache entry — ACL scope is not isolated in the cache key")
	}
	results = data["results"].([]any)
	if len(results) != 0 {
		t.Fatalf("member results = %+v, want empty (owner_only doc, and the owner's cache entry must not leak it)", results)
	}
}

// countingVectorIndex counts real Query calls — used to prove a cache hit
// actually skips Pinecone rather than merely returning the same data.
type countingVectorIndex struct {
	fakeVectorIndex
	calls *int
}

func (c *countingVectorIndex) Namespace(ns string) coredocs.VectorIndex { return c }
func (c *countingVectorIndex) Query(ctx context.Context, vector []float32, topK uint32, filter *pinecone.MetadataFilter) ([]coredocs.QueryMatch, error) {
	*c.calls++
	return c.fakeVectorIndex.Query(ctx, vector, topK, filter)
}

// testOrgAndUserAsMemberOf adds a second, member-role user to an existing
// org — the cache-isolation test needs 2 different-role users in the SAME
// org (cache keys are scoped by org_id, so 2 separate orgs wouldn't
// exercise the same cache key at all).
func testOrgAndUserAsMemberOf(t *testing.T, systemPool *pgxpool.Pool, orgID pgtype.UUID) (pgtype.UUID, string) {
	t.Helper()
	suffix := randSuffix(t)
	clerkUserID := "user_documents_test_member_" + suffix
	email := "member-" + suffix + "@example.com"
	_, err := systemPool.Exec(context.Background(),
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, $3, 'member')`,
		orgID, clerkUserID, email,
	)
	if err != nil {
		t.Fatalf("insert second test user: %v", err)
	}
	return orgID, clerkUserID
}
