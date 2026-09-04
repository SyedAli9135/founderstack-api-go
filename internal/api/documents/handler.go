// Package documents implements the upload/list/get/delete/reindex HTTP endpoints over
// internal/core/documents' S3/extract/chunk/embed/index pipeline.
package documents

import (
	"context"
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

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	coredocs "github.com/founderstack/api/internal/core/documents"
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

type Handler struct {
	appPool   *pgxpool.Pool
	store     coredocs.BlobStore
	processor *coredocs.Processor
}

// store takes the BlobStore interface, not the concrete *coredocs.Store, so tests can
// inject a fake instead of needing a real S3/LocalStack dependency.
func NewHandler(appPool *pgxpool.Pool, store coredocs.BlobStore, processor *coredocs.Processor) *Handler {
	return &Handler{appPool: appPool, store: store, processor: processor}
}

// Register mounts all 5 routes; rg must already have middleware.RequireAuth applied.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/documents/upload", h.Upload)
	rg.GET("/documents", h.List)
	rg.GET("/documents/:id", h.Get)
	rg.DELETE("/documents/:id", h.Delete)
	rg.POST("/documents/:id/reindex", h.Reindex)
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
