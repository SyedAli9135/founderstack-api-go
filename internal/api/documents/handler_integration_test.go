//go:build integration

package documents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	coredocs "github.com/founderstack/api/internal/core/documents"
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

type fakeVectorIndex struct{}

func (fakeVectorIndex) Namespace(ns string) coredocs.VectorIndex { return fakeVectorIndex{} }
func (fakeVectorIndex) Upsert(ctx context.Context, vectors []*pinecone.Vector) error {
	return nil
}
func (fakeVectorIndex) DeleteByID(ctx context.Context, ids []string) error { return nil }

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

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, store coredocs.BlobStore, processor *coredocs.Processor) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	h := NewHandler(appPool, store, processor)
	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	h.Register(authed)

	return r
}

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
