package documents

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// embedModel is deliberately the multilingual variant, not cmd/seedtools'
// embed-english-v3.0: tool descriptions are English-only by construction,
// but a founder's uploaded documents aren't guaranteed to be.
const embedModel = "embed-multilingual-v3.0"

// embedBatchSize: Cohere's Embed API caps a single request at 96 texts,
// not 100 — a batch of exactly 100 gets rejected once it actually fills.
const embedBatchSize = 96

// Processor runs the actual upload -> extract -> chunk -> embed -> index
// pipeline (Process) and is reused by the boot-time recovery sweep
// (recover.go) for documents whose processing goroutine was lost to a
// process restart.
type Processor struct {
	appPool  *pgxpool.Pool
	store    BlobStore
	embedder Embedder
	index    VectorIndex // base connection to founderstack-rag; namespace is set per-org via Namespace
}

func NewProcessor(appPool *pgxpool.Pool, store BlobStore, embedder Embedder, index VectorIndex) *Processor {
	return &Processor{appPool: appPool, store: store, embedder: embedder, index: index}
}

// namespaceForOrg matches workflow 6's spec: each org's chunks live in
// their own Pinecone namespace, so RLS-style tenant isolation extends
// into the vector store too — a query scoped to the wrong namespace
// structurally cannot return another org's chunks, not just "shouldn't."
func namespaceForOrg(orgID pgtype.UUID) string {
	return "org_" + orgID.String()
}

// Process runs the full pipeline for one document. On any failure, the
// document is marked 'failed' with the error detail rather than left
// stuck at 'processing' — a founder retrying via POST .../reindex needs
// something to look at.
func (p *Processor) Process(ctx context.Context, orgID, docID pgtype.UUID) error {
	var doc dbgen.GetDocumentRow
	err := tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetDocument(ctx, dbgen.GetDocumentParams{OrgID: orgID, ID: docID})
		if err != nil {
			return err
		}
		doc = row
		status := "processing"
		return q.UpdateDocumentProcessing(ctx, dbgen.UpdateDocumentProcessingParams{OrgID: orgID, ID: docID, ProcessingStatus: &status})
	})
	if err != nil {
		return fmt.Errorf("documents: load document %s: %w", docID.String(), err)
	}

	if procErr := p.run(ctx, orgID, docID, doc); procErr != nil {
		detail := procErr.Error()
		if markErr := tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
			return q.MarkDocumentFailed(ctx, dbgen.MarkDocumentFailedParams{OrgID: orgID, ID: docID, ErrorDetail: &detail})
		}); markErr != nil {
			return fmt.Errorf("documents: process failed (%w) and mark-failed also failed: %w", procErr, markErr)
		}
		return procErr
	}
	return nil
}

func (p *Processor) run(ctx context.Context, orgID, docID pgtype.UUID, doc dbgen.GetDocumentRow) error {
	rc, err := p.store.Download(ctx, doc.S3Path)
	if err != nil {
		return fmt.Errorf("download from s3: %w", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read downloaded file: %w", err)
	}

	text, err := ExtractText(doc.Filename, data)
	if err != nil {
		return fmt.Errorf("extract text: %w", err)
	}

	chunks := SplitText(text, DefaultChunkSize, DefaultChunkOverlap)
	if len(chunks) == 0 {
		return fmt.Errorf("no extractable text in %s", doc.Filename)
	}

	idxConn := p.index.Namespace(namespaceForOrg(orgID))
	docIDStr := docID.String()
	total := 0

	for start := 0; start < len(chunks); start += embedBatchSize {
		end := min(start+embedBatchSize, len(chunks))
		batch := chunks[start:end]

		embeddings, err := p.embedder.Embed(ctx, batch)
		if err != nil {
			return fmt.Errorf("embed chunks %d-%d: %w", start, end, err)
		}
		if len(embeddings) != len(batch) {
			return fmt.Errorf("cohere returned %d embeddings for %d chunks", len(embeddings), len(batch))
		}

		vectors := make([]*pinecone.Vector, len(batch))
		pineconeIDs := make([]string, len(batch))
		for i, embedding := range embeddings {
			chunkIndex := start + i
			pineconeID := fmt.Sprintf("%s-%d", docIDStr, chunkIndex)
			pineconeIDs[i] = pineconeID

			values := make([]float32, len(embedding))
			for j, v := range embedding {
				values[j] = float32(v)
			}
			metadata, err := structpb.NewStruct(map[string]any{
				"doc_id":      docIDStr,
				"chunk_index": chunkIndex,
				"text":        batch[i],
			})
			if err != nil {
				return fmt.Errorf("build vector metadata: %w", err)
			}
			vectors[i] = &pinecone.Vector{Id: pineconeID, Values: &values, Metadata: metadata}
		}

		if err := idxConn.Upsert(ctx, vectors); err != nil {
			return fmt.Errorf("upsert vectors to pinecone: %w", err)
		}

		err = tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
			for i, pid := range pineconeIDs {
				if err := q.InsertDocumentChunk(ctx, dbgen.InsertDocumentChunkParams{
					DocID:      docID,
					ChunkIndex: int32(start + i),
					PineconeID: pid,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("insert document_chunks rows: %w", err)
		}

		total += len(batch)
	}

	totalChunks := int32(total)
	return tenant.WithTx(ctx, p.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.MarkDocumentIndexed(ctx, dbgen.MarkDocumentIndexedParams{OrgID: orgID, ID: docID, TotalChunks: &totalChunks})
	})
}
