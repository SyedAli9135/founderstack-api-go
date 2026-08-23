package documents

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// Reindex clears a document's existing chunks/vectors and reprocesses it
// from scratch (POST .../{id}/reindex) — a founder re-embedding with an
// updated model, or retrying after a 'failed' status without
// re-uploading the file.
func (p *Processor) Reindex(ctx context.Context, orgID, docID pgtype.UUID) error {
	var pineconeIDs []string
	err := tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		ids, err := q.ListDocumentChunkPineconeIDs(ctx, docID)
		if err != nil {
			return err
		}
		pineconeIDs = ids
		if err := q.DeleteDocumentChunks(ctx, docID); err != nil {
			return err
		}
		status := "pending"
		return q.UpdateDocumentProcessing(ctx, dbgen.UpdateDocumentProcessingParams{OrgID: orgID, ID: docID, ProcessingStatus: &status})
	})
	if err != nil {
		return fmt.Errorf("documents: clear existing chunks for reindex: %w", err)
	}

	if len(pineconeIDs) > 0 {
		idxConn := p.index.Namespace(namespaceForOrg(orgID))
		if err := retryOnce(func() error { return idxConn.DeleteByID(ctx, pineconeIDs) }); err != nil {
			// Not fatal to the reindex itself: the pinecone_id scheme is
			// {doc_id}-{chunk_index}, so as long as this document
			// re-chunks to at least as many chunks as before, every old
			// id gets overwritten by Process's own upsert anyway. It's
			// only if reprocessing produces *fewer* chunks that a
			// leftover old vector could linger — logged so it's visible,
			// not silently swallowed, but not worth blocking the whole
			// reindex over.
			slog.Warn("documents: reindex could not delete old vectors, continuing", "doc_id", docID.String(), "error", err)
		}
	}

	return p.Process(ctx, orgID, docID)
}
