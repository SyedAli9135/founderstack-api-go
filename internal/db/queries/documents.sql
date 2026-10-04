-- Queries backing workflow 6 (document upload / RAG). Tenant-scoped reads
-- and writes run through app_user via tenant.WithTx, same convention as
-- every other feature area in this file set. The recovery-sweep query
-- (ListStuckDocuments) is the one deliberate exception — it scans across
-- every org's documents, so it runs on app_system, matching
-- internal/core/integrations/refresh.go's RunRefreshJob precedent for
-- the same "this is inherently cross-tenant" reasoning.

-- name: InsertDocument :exec
-- id is application-generated (not the column's gen_random_uuid()
-- default) so the handler knows the S3 key (documents/{org_id}/{doc_id}/{filename})
-- before uploading, rather than uploading first and updating s3_path
-- after — one INSERT instead of an insert-then-update dance.
INSERT INTO documents (id, org_id, filename, s3_path, mime_type, byte_size, category, processing_status, uploaded_by, visibility)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9);

-- name: ListDocuments :many
-- Excludes 'deleting': once DELETE .../{id} has been called, the
-- document shouldn't reappear in a normal list view while
-- purgeDocumentJob finishes removing it (the row itself is only ever
-- hard-deleted after that succeeds — see HardDeleteDocument).
-- include_owner_only is the caller's own owner/admin check, same as search:
-- an owner_only document's name and status are hidden from everyone else.
SELECT id, filename, category, processing_status, total_chunks, byte_size, created_at, indexed_at, visibility, uploaded_by
FROM documents
WHERE org_id = sqlc.arg(org_id) AND processing_status != 'deleting'
  AND (visibility = 'all_members' OR sqlc.arg(include_owner_only)::bool)
ORDER BY created_at DESC;

-- name: GetDocument :one
SELECT id, filename, s3_path, mime_type, byte_size, category, processing_status, total_chunks, indexed_at, error_detail, created_at, visibility
FROM documents
WHERE org_id = $1 AND id = $2;

-- name: GetDocumentForUser :one
-- The API-facing lookup: an owner_only document doesn't exist as far as a
-- non-admin caller is concerned. The pipeline itself uses GetDocument.
SELECT id, filename, category, processing_status, total_chunks, byte_size, created_at, indexed_at, error_detail, visibility, uploaded_by
FROM documents
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
  AND (visibility = 'all_members' OR sqlc.arg(include_owner_only)::bool);

-- Workflow 12 (RAG search). ListSearchableDocumentIDs is the ACL + category
-- filter, resolved *before* any embedding/Pinecone call so a query that
-- can't match anything (e.g. a member with only owner_only docs uploaded)
-- skips the expensive calls entirely. include_owner_only is the
-- requesting user's own role check (role IN ('owner','admin')), computed
-- in Go, not SQL — see internal/api/documents/handler.go's Search.

-- name: ListSearchableDocumentIDs :many
SELECT id FROM documents
WHERE org_id = $1
  AND processing_status = 'indexed'
  AND (visibility = 'all_members' OR sqlc.arg(include_owner_only)::bool)
  AND (sqlc.narg(category)::varchar IS NULL OR category = sqlc.narg(category));

-- name: GetDocumentsByIDs :many
-- Batch-hydrates a page of search results' filename/category — Pinecone's
-- own vector metadata only carries doc_id/chunk_index/text (see
-- processor.go), not display fields.
SELECT id, filename, category FROM documents
WHERE org_id = $1 AND id = ANY(sqlc.arg(doc_ids)::uuid[]);

-- name: UpdateDocumentProcessing :exec
UPDATE documents SET processing_status = $3 WHERE org_id = $1 AND id = $2;

-- name: ClaimDocumentForProcessing :execrows
-- Takes a document for processing: either it is waiting ('pending'), or it
-- says 'processing' but hasn't been touched since stale_before — its worker
-- died. A document another instance is actively working on (fresh
-- 'processing') yields no row, so the boot-time recovery sweep on a second
-- instance can't start it a second time and pay to embed it twice.
UPDATE documents SET processing_status = 'processing'
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id)
  AND (processing_status = 'pending'
       OR (processing_status = 'processing' AND updated_at < sqlc.arg(stale_before)));

-- name: TouchDocument :exec
-- A heartbeat while a long document is processed, so it isn't mistaken for a
-- dead job (the updated_at trigger does the work).
UPDATE documents SET processing_status = processing_status WHERE org_id = $1 AND id = $2;

-- name: MarkDocumentIndexed :exec
UPDATE documents
SET processing_status = 'indexed', total_chunks = $3, indexed_at = now(), error_detail = NULL
WHERE org_id = $1 AND id = $2;

-- name: MarkDocumentFailed :exec
UPDATE documents SET processing_status = 'failed', error_detail = $3 WHERE org_id = $1 AND id = $2;

-- name: SoftDeleteDocument :exec
UPDATE documents SET processing_status = 'deleting' WHERE org_id = $1 AND id = $2;

-- name: HardDeleteDocument :exec
-- Only called after purgeDocumentJob has successfully removed the
-- Pinecone vectors and the S3 object — see internal/core/documents/purge.go.
DELETE FROM documents WHERE org_id = $1 AND id = $2;

-- name: InsertDocumentChunk :exec
INSERT INTO document_chunks (doc_id, chunk_index, pinecone_id) VALUES ($1, $2, $3);

-- name: ListDocumentChunkPineconeIDs :many
SELECT pinecone_id FROM document_chunks WHERE doc_id = $1;

-- name: DeleteDocumentChunks :exec
DELETE FROM document_chunks WHERE doc_id = $1;

-- name: ListStuckDocuments :many
-- Documents whose background job (processDocument or purgeDocumentJob)
-- may have been running in a process that restarted mid-job — the
-- goroutine-based tradeoff internal/core/documents.RecoverStuckJobs
-- exists to cover. olderThan guards against re-kicking a job that's
-- still genuinely in flight in *this* process, not actually stuck.
SELECT id, org_id, processing_status
FROM documents
WHERE processing_status IN ('pending', 'processing', 'deleting')
  AND updated_at < $1;
