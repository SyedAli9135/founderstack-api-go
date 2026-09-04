package documents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// Purge is purgeDocumentJob: deletes a document's Pinecone vectors, then
// its S3 object, then its Postgres rows — external deletions first, DB
// rows last, and only if both externals actually succeeded. Deleting
// rows before confirming the external cleanup would leave orphaned
// vectors/files with no record pointing at them; getting the order
// backwards is the whole reason this isn't just one DELETE statement.
func (p *Processor) Purge(ctx context.Context, orgID, docID pgtype.UUID) error {
	var doc dbgen.GetDocumentRow
	var pineconeIDs []string
	err := tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetDocument(ctx, dbgen.GetDocumentParams{OrgID: orgID, ID: docID})
		if err != nil {
			return err
		}
		doc = row
		ids, err := q.ListDocumentChunkPineconeIDs(ctx, docID)
		if err != nil {
			return err
		}
		pineconeIDs = ids
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already gone — purge is idempotent, not an error
		}
		return fmt.Errorf("documents: load document %s for purge: %w", docID.String(), err)
	}

	if len(pineconeIDs) > 0 {
		idxConn := p.index.Namespace(namespaceForOrg(orgID))
		if err := retryOnce(func() error { return idxConn.DeleteByID(ctx, pineconeIDs) }); err != nil {
			return fmt.Errorf("documents: delete pinecone vectors for %s: %w", docID.String(), err)
		}
	}

	if err := retryOnce(func() error { return p.store.Delete(ctx, doc.S3Path) }); err != nil {
		return fmt.Errorf("documents: delete s3 object %s: %w", doc.S3Path, err)
	}

	return tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		if err := q.DeleteDocumentChunks(ctx, docID); err != nil {
			return err
		}
		return q.HardDeleteDocument(ctx, dbgen.HardDeleteDocumentParams{OrgID: orgID, ID: docID})
	})
}

// retryOnce: try once, retry once on failure, then give up — not a full
// backoff loop. A transient failure gets one more chance; a persistent one
// surfaces immediately.
func retryOnce(fn func() error) error {
	if err := fn(); err == nil {
		return nil
	}
	return fn()
}
