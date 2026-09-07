package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	coredocs "github.com/founderstack/api/internal/core/documents"
	"github.com/founderstack/api/internal/core/llm"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

const maxUploadBytes = 50 << 20

// Checked here at the API boundary; internal/core/documents.ExtractText independently
// rejects anything else too, so a file that skips this check still can't be mis-processed.
var allowedExtensions = map[string]string{
	".pdf":  "application/pdf",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".txt":  "text/plain",
	".md":   "text/markdown",
}

// chatClientResolver matches llm.ResolveChatClient's signature — an
// injectable test seam, same pattern graph.NewLauncherWithResolver uses.
type chatClientResolver func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error)

type Handler struct {
	appPool           *pgxpool.Pool
	store             coredocs.BlobStore
	processor         *coredocs.Processor
	searcher          *coredocs.Searcher
	redis             *redis.Client
	encryptionKey     []byte
	resolveChatClient chatClientResolver
}

// store takes the BlobStore interface, not the concrete *coredocs.Store, so tests can
// inject a fake instead of needing a real S3/LocalStack dependency.
func NewHandler(appPool *pgxpool.Pool, store coredocs.BlobStore, processor *coredocs.Processor, searcher *coredocs.Searcher, rdb *redis.Client, encryptionKey []byte) *Handler {
	return NewHandlerWithResolver(appPool, store, processor, searcher, rdb, encryptionKey, llm.ResolveChatClient)
}

// NewHandlerWithResolver is NewHandler with an injectable ChatClient
// resolver — tests use it to substitute a fake HyDE response without a
// real BYOK key.
func NewHandlerWithResolver(appPool *pgxpool.Pool, store coredocs.BlobStore, processor *coredocs.Processor, searcher *coredocs.Searcher, rdb *redis.Client, encryptionKey []byte, resolveChatClient chatClientResolver) *Handler {
	return &Handler{
		appPool: appPool, store: store, processor: processor, searcher: searcher,
		redis: rdb, encryptionKey: encryptionKey, resolveChatClient: resolveChatClient,
	}
}

// Register mounts all 6 routes; rg must already have middleware.RequireAuth applied.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/documents/upload", h.Upload)
	rg.GET("/documents", h.List)
	rg.GET("/documents/:id", h.Get)
	rg.DELETE("/documents/:id", h.Delete)
	rg.POST("/documents/:id/reindex", h.Reindex)
	rg.POST("/documents/search", h.Search)
}

// Upload returns 202 immediately; the founder polls GET .../{id} for processing_status.
func (h *Handler) Upload(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "file is required")
		return
	}
	if fileHeader.Size > maxUploadBytes {
		response.Fail(c, http.StatusBadRequest, "FILE_TOO_LARGE", "File exceeds the 50MB limit")
		return
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	mimeType, ok := allowedExtensions[ext]
	if !ok {
		response.Fail(c, http.StatusBadRequest, "UNSUPPORTED_FILE_TYPE", "Supported formats: PDF, DOCX, TXT, MD")
		return
	}

	category := c.PostForm("category")
	if category == "" {
		category = "general"
	}

	visibility := c.PostForm("visibility")
	if visibility == "" {
		visibility = "all_members"
	}
	if visibility != "all_members" && visibility != "owner_only" {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "visibility must be all_members or owner_only")
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read uploaded file")
		return
	}
	defer file.Close()

	// Generated here, not left to the DB default, so the S3 key is known before uploading —
	// avoids an insert-then-update-s3_path round trip.
	docID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	key := coredocs.Key(user.OrgID.String(), docID.String(), fileHeader.Filename)

	ctx := c.Request.Context()
	if err := h.store.Upload(ctx, key, file, mimeType); err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not store the uploaded file")
		return
	}

	byteSize := int32(fileHeader.Size)
	err = tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertDocument(ctx, dbgen.InsertDocumentParams{
			ID:         docID,
			OrgID:      user.OrgID,
			Filename:   fileHeader.Filename,
			S3Path:     key,
			MimeType:   &mimeType,
			ByteSize:   &byteSize,
			Category:   &category,
			UploadedBy: user.ID,
			Visibility: visibility,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not save the document record")
		return
	}

	orgID := user.OrgID
	go func() {
		// Not c.Request.Context(): that's cancelled the moment this handler returns.
		if err := h.processor.Process(context.Background(), orgID, docID); err != nil {
			// Process already persists 'failed'+error_detail; this log is for operators only.
			logProcessingError("upload", docID, err)
		}
	}()

	response.OK(c, http.StatusAccepted, "Document uploaded, processing started",
		gin.H{"doc_id": docID.String(), "status": "processing"})
}

type documentSummary struct {
	ID               string     `json:"id"`
	Filename         string     `json:"filename"`
	Category         string     `json:"category"`
	ProcessingStatus string     `json:"processing_status"`
	TotalChunks      int32      `json:"total_chunks"`
	ByteSize         int32      `json:"byte_size"`
	CreatedAt        time.Time  `json:"created_at"`
	IndexedAt        *time.Time `json:"indexed_at,omitempty"`
	Visibility       string     `json:"visibility"`
}

func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var docs []documentSummary
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListDocuments(ctx, user.OrgID)
		if err != nil {
			return err
		}
		docs = make([]documentSummary, 0, len(rows))
		for _, row := range rows {
			docs = append(docs, documentSummary{
				ID:               row.ID.String(),
				Filename:         row.Filename,
				Category:         derefOr(row.Category, "general"),
				ProcessingStatus: derefOr(row.ProcessingStatus, "pending"),
				TotalChunks:      derefInt32(row.TotalChunks),
				ByteSize:         derefInt32(row.ByteSize),
				CreatedAt:        row.CreatedAt.Time,
				IndexedAt:        timestamptzPtr(row.IndexedAt),
				Visibility:       row.Visibility,
			})
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list documents")
		return
	}

	response.OK(c, http.StatusOK, "", docs)
}

type documentDetail struct {
	documentSummary
	ErrorDetail *string `json:"error_detail,omitempty"`
}

// RLS makes "wrong org" indistinguishable from "doesn't exist", so both 404 here.
func (h *Handler) Get(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	docID, ok := parseDocID(c)
	if !ok {
		return
	}

	var doc documentDetail
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetDocument(ctx, dbgen.GetDocumentParams{OrgID: user.OrgID, ID: docID})
		if err != nil {
			return err
		}
		doc = documentDetail{
			documentSummary: documentSummary{
				ID:               row.ID.String(),
				Filename:         row.Filename,
				Category:         derefOr(row.Category, "general"),
				ProcessingStatus: derefOr(row.ProcessingStatus, "pending"),
				TotalChunks:      derefInt32(row.TotalChunks),
				ByteSize:         derefInt32(row.ByteSize),
				CreatedAt:        row.CreatedAt.Time,
				IndexedAt:        timestamptzPtr(row.IndexedAt),
				Visibility:       row.Visibility,
			},
			ErrorDetail: row.ErrorDetail,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "DOCUMENT_NOT_FOUND", "Document not found")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch document")
		return
	}

	response.OK(c, http.StatusOK, "", doc)
}

// Soft-deletes and kicks off background purging, matching this codebase's other
// delete-ish endpoints: mark it, don't destroy it synchronously in the request path.
func (h *Handler) Delete(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	docID, ok := parseDocID(c)
	if !ok {
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.SoftDeleteDocument(ctx, dbgen.SoftDeleteDocumentParams{OrgID: user.OrgID, ID: docID})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not delete document")
		return
	}

	orgID := user.OrgID
	go func() {
		if err := h.processor.Purge(context.Background(), orgID, docID); err != nil {
			logProcessingError("delete", docID, err)
		}
	}()

	c.Status(http.StatusNoContent)
}

func (h *Handler) Reindex(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	docID, ok := parseDocID(c)
	if !ok {
		return
	}

	// Confirm existence before 202 — an unknown id should 404, not silently no-op.
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.GetDocument(ctx, dbgen.GetDocumentParams{OrgID: user.OrgID, ID: docID})
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "DOCUMENT_NOT_FOUND", "Document not found")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch document")
		return
	}

	orgID := user.OrgID
	go func() {
		if err := h.processor.Reindex(context.Background(), orgID, docID); err != nil {
			logProcessingError("reindex", docID, err)
		}
	}()

	response.OK(c, http.StatusAccepted, "Reindexing started", gin.H{"doc_id": docID.String(), "status": "processing"})
}

func parseDocID(c *gin.Context) (pgtype.UUID, bool) {
	raw := c.Param("id")
	parsed, err := uuid.Parse(raw)
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid document id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func derefInt32(n *int32) int32 {
	if n == nil {
		return 0
	}
	return *n
}

func timestamptzPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func logProcessingError(op string, docID pgtype.UUID, err error) {
	fmt.Printf("documents: background %s failed for %s: %v\n", op, docID.String(), err)
}

// hydeModelByProvider: HyDE only needs a short, cheap completion (a
// plausible hypothetical passage, not real reasoning) — each provider's
// fastest/cheapest publicly available chat model, not whatever model an
// agent happens to be configured with (search isn't agent-scoped).
var hydeModelByProvider = map[llm.ProviderID]string{
	llm.ProviderAnthropic: "claude-3-5-haiku-20241022",
	llm.ProviderOpenAI:    "gpt-4o-mini",
	llm.ProviderGemini:    "gemini-2.0-flash",
	llm.ProviderQwen:      "qwen-turbo",
	llm.ProviderDeepSeek:  "deepseek-chat",
}

// hydeSystemPrompt asks for brevity in the prompt itself — llm.ChatClient
// has no per-call max-tokens override (internal/core/llm/chat_anthropic.go
// fixes anthropicMaxTokens=4096 for every call), so length is
// prompt-enforced here rather than API-enforced.
const hydeSystemPrompt = "You are helping retrieve information from a document knowledge base. " +
	"Given a question, write a short, plausible hypothetical passage (2-3 sentences, well under 100 words) " +
	"that might appear in a real document answering it. Write only the passage itself, in a natural, " +
	"factual tone — no preamble, no caveats, no mention that it's hypothetical."

// chatClientHyDE adapts a resolved llm.ChatClient to coredocs.HyDEGenerator
// — built fresh per search request once the org's provider/key is known,
// not a Searcher field (see Searcher.Search's doc comment).
type chatClientHyDE struct {
	client llm.ChatClient
}

func (h chatClientHyDE) GenerateHypotheticalAnswer(ctx context.Context, query string) (string, error) {
	resp, err := h.client.Send(ctx, hydeSystemPrompt, []llm.Message{{Role: llm.RoleUser, Content: query}}, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// resolveHyDE is best-effort: no configured provider, no active key, or
// any resolution error all mean "skip HyDE," never "fail the search" — a
// founder without a BYOK key configured can still search their documents,
// just without the retrieval-quality boost. See Searcher.Search's own
// graceful-degradation handling of a nil/failing HyDEGenerator.
func (h *Handler) resolveHyDE(ctx context.Context, orgID pgtype.UUID) coredocs.HyDEGenerator {
	var provider *string
	err := tenant.WithTx(ctx, h.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		settings, err := q.GetOrgRunSettings(ctx, orgID)
		if err != nil {
			return err
		}
		provider = settings.LlmProvider
		return nil
	})
	if err != nil || provider == nil || *provider == "" {
		return nil
	}
	model, ok := hydeModelByProvider[llm.ProviderID(*provider)]
	if !ok {
		return nil
	}
	client, err := h.resolveChatClient(ctx, h.appPool, h.encryptionKey, orgID, llm.ProviderID(*provider), model)
	if err != nil {
		return nil
	}
	return chatClientHyDE{client: client}
}

type searchRequest struct {
	Query    string  `json:"query"`
	Category *string `json:"category,omitempty"`
	TopK     *int    `json:"top_k,omitempty"`
}

type searchResult struct {
	Content        string  `json:"content"`
	DocFilename    string  `json:"doc_filename"`
	Category       string  `json:"category"`
	RelevanceScore float64 `json:"relevance_score"`
	ChunkIndex     int     `json:"chunk_index"`
}

const (
	defaultSearchTopK = 5
	maxSearchTopK     = 20
	searchCacheTTL    = time.Hour
)

// searchCacheKey follows WORKFLOW_PLAN_GO.md's own convention
// (cache:rag:{org_id}:{sha256(query+category)}) with one addition: the
// requesting user's ACL scope is folded into the hash too. Without that,
// an owner's search populating the cache could leak an owner_only
// document's content to a member who later issues the identical
// query+category — the literal plan spec would cache across roles and
// leak cross-role content. Caught during implementation, not after.
func searchCacheKey(orgID pgtype.UUID, canSeeOwnerOnly bool, query string, category *string) string {
	h := sha256.New()
	h.Write([]byte(query))
	if category != nil {
		h.Write([]byte(*category))
	}
	if canSeeOwnerOnly {
		h.Write([]byte{1})
	}
	return "cache:rag:" + orgID.String() + ":" + hex.EncodeToString(h.Sum(nil))
}

// Search implements workflow 12 (RAG query): Redis cache check -> resolve
// which documents this user is allowed to see (ACL + category) -> HyDE ->
// embed -> Pinecone query -> rerank -> hydrate -> cache -> audit log.
func (h *Handler) Search(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var req searchRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Query) == "" {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "query is required")
		return
	}
	topK := defaultSearchTopK
	if req.TopK != nil && *req.TopK > 0 && *req.TopK <= maxSearchTopK {
		topK = *req.TopK
	}

	ctx := c.Request.Context()
	canSeeOwnerOnly := user.Role == "owner" || user.Role == "admin"
	cacheKey := searchCacheKey(user.OrgID, canSeeOwnerOnly, req.Query, req.Category)

	if h.redis != nil {
		if cached, err := h.redis.Get(ctx, cacheKey).Bytes(); err == nil {
			var results []searchResult
			if err := json.Unmarshal(cached, &results); err == nil {
				h.auditSearch(ctx, user, req.Category, results, true)
				response.OK(c, http.StatusOK, "", gin.H{"results": results, "from_cache": true})
				return
			}
		}
	}

	var allowedIDs []pgtype.UUID
	err := tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		allowedIDs, err = q.ListSearchableDocumentIDs(ctx, dbgen.ListSearchableDocumentIDsParams{
			OrgID: user.OrgID, IncludeOwnerOnly: canSeeOwnerOnly, Category: req.Category,
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not resolve searchable documents")
		return
	}
	if len(allowedIDs) == 0 {
		// Nothing this user is allowed to see matches — skip the
		// HyDE/embed/Pinecone calls entirely rather than searching a
		// namespace we're just going to filter down to nothing anyway.
		// Still audited: a search that matched nothing because of ACL is
		// exactly the kind of attempt a compliance-minded founder cares
		// about seeing, not less interesting than a successful one.
		h.auditSearch(ctx, user, req.Category, nil, false)
		response.OK(c, http.StatusOK, "", gin.H{"results": []searchResult{}, "from_cache": false})
		return
	}

	docIDStrs := make([]any, len(allowedIDs))
	for i, id := range allowedIDs {
		docIDStrs[i] = uuid.UUID(id.Bytes).String()
	}
	filter, err := pinecone.NewMetadataFilter(map[string]any{"doc_id": map[string]any{"$in": docIDStrs}})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not build search filter")
		return
	}

	hyde := h.resolveHyDE(ctx, user.OrgID)
	chunks, err := h.searcher.Search(ctx, user.OrgID, req.Query, hyde, filter, topK)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Search failed")
		return
	}

	results := make([]searchResult, 0, len(chunks))
	if len(chunks) > 0 {
		docMeta := map[string]dbgen.GetDocumentsByIDsRow{}
		err := tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
			rows, err := q.GetDocumentsByIDs(ctx, dbgen.GetDocumentsByIDsParams{OrgID: user.OrgID, DocIds: uniqueDocIDs(chunks)})
			if err != nil {
				return err
			}
			for _, row := range rows {
				docMeta[row.ID.String()] = row
			}
			return nil
		})
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not hydrate search results")
			return
		}
		for _, chunk := range chunks {
			meta := docMeta[chunk.DocID]
			results = append(results, searchResult{
				Content: chunk.Text, DocFilename: meta.Filename, Category: derefOr(meta.Category, "general"),
				RelevanceScore: chunk.RelevanceScore, ChunkIndex: chunk.ChunkIndex,
			})
		}
	}

	if h.redis != nil {
		if payload, err := json.Marshal(results); err == nil {
			if err := h.redis.Set(ctx, cacheKey, payload, searchCacheTTL).Err(); err != nil {
				fmt.Printf("documents: cache search result failed: %v\n", err)
			}
		}
	}

	h.auditSearch(ctx, user, req.Category, results, false)
	response.OK(c, http.StatusOK, "", gin.H{"results": results, "from_cache": false})
}

// auditSearch logs every search attempt — cache hit, ACL-empty, and the
// full path alike, so a compliance-minded founder sees a contractor's
// zero-result attempt at an owner_only document too, not just successful
// searches. No content stored: the query text itself isn't audit-log
// material, per WORKFLOW_PLAN_GO.md's own instruction. Best-effort: an
// audit-log write failure shouldn't fail a search that already succeeded.
//
// avg_rerank_score/from_cache were added for workflow 14's rag-quality
// analytics endpoint, which has no other source for this data — the HTTP
// response itself is the only other place a rerank score ever appears,
// and it's never persisted anywhere else.
func (h *Handler) auditSearch(ctx context.Context, user authctx.User, category *string, results []searchResult, fromCache bool) {
	metadata, _ := json.Marshal(map[string]any{
		"result_count": len(results), "category": category,
		"avg_rerank_score": avgRelevanceScore(results), "from_cache": fromCache,
	})
	_ = tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertAuditLog(ctx, dbgen.InsertAuditLogParams{
			OrgID: user.OrgID, ActorID: user.ID, ActorType: "user",
			Action: "rag.search", MetadataInfo: metadata,
		})
	})
}

func avgRelevanceScore(results []searchResult) float64 {
	if len(results) == 0 {
		return 0
	}
	var sum float64
	for _, r := range results {
		sum += r.RelevanceScore
	}
	return sum / float64(len(results))
}

func uniqueDocIDs(chunks []coredocs.SearchChunk) []pgtype.UUID {
	seen := make(map[string]bool, len(chunks))
	out := make([]pgtype.UUID, 0, len(chunks))
	for _, c := range chunks {
		if seen[c.DocID] {
			continue
		}
		seen[c.DocID] = true
		parsed, err := uuid.Parse(c.DocID)
		if err != nil {
			continue // malformed doc_id in vector metadata shouldn't crash the whole search response
		}
		out = append(out, pgtype.UUID{Bytes: parsed, Valid: true})
	}
	return out
}
