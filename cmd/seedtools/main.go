// Command seedtools embeds every registered MCP tool's name+description+
// input schema via Cohere and upserts the vectors into Pinecone's
// founderstack-tools index (namespace "tools") — workflow 5's tool
// discovery step. The planner (workflow 9, not yet built) will query this
// index to semantically find relevant tools for a task instead of
// listing all of them in every prompt.
//
// Run: go run ./cmd/seedtools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	cohere "github.com/cohere-ai/cohere-go/v2"
	coherecli "github.com/cohere-ai/cohere-go/v2/client"
	coreoption "github.com/cohere-ai/cohere-go/v2/option"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pinecone-io/go-pinecone/v5/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/founderstack/api/internal/config"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/core/mcp/servers"
)

// embedModel is Cohere's v3 text embedding model — 1024-dimensional
// output, matching the founderstack-tools index's configured dimension
// (see WORKFLOW_PLAN_GO.md workflow 1's acceptance criteria: both
// indexes created at dim=1024).
const embedModel = "embed-english-v3.0"

func main() {
	if err := run(); err != nil {
		slog.Error("seedtools failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	registry, err := coremcp.NewRegistry(ctx, servers.AllServers())
	if err != nil {
		return fmt.Errorf("build tool registry: %w", err)
	}

	toolsByService, err := registry.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	type toolRecord struct {
		service string
		tool    *gomcp.Tool
	}
	var records []toolRecord
	var texts []string
	for service, tools := range toolsByService {
		for _, tool := range tools {
			records = append(records, toolRecord{service: service, tool: tool})
			texts = append(texts, embeddingText(service, tool))
		}
	}
	if len(records) == 0 {
		return fmt.Errorf("no tools registered — nothing to embed")
	}
	logger.Info("collected tools", "count", len(records))

	cohereClient := coherecli.NewClient(coreoption.WithToken(cfg.CohereAPIKey.Expose()))
	inputType := cohere.EmbedInputTypeSearchDocument
	embedResp, err := cohereClient.Embed(ctx, &cohere.EmbedRequest{
		Texts:     texts,
		Model:     stringPtr(embedModel),
		InputType: &inputType,
	})
	if err != nil {
		return fmt.Errorf("embed tools via cohere: %w", err)
	}
	floats := embedResp.GetEmbeddingsFloats()
	if floats == nil || len(floats.Embeddings) != len(records) {
		return fmt.Errorf("cohere returned %d embeddings for %d tools", len(floats.GetEmbeddings()), len(records))
	}

	pc, err := pinecone.NewClient(pinecone.NewClientParams{ApiKey: cfg.PineconeAPIKey.Expose()})
	if err != nil {
		return fmt.Errorf("build pinecone client: %w", err)
	}
	idx, err := pc.DescribeIndex(ctx, cfg.PineconeIndexTools)
	if err != nil {
		return fmt.Errorf("describe pinecone index %q: %w", cfg.PineconeIndexTools, err)
	}
	const toolsNamespace = "tools"
	idxConn, err := pc.Index(pinecone.NewIndexConnParams{Host: idx.Host, Namespace: toolsNamespace})
	if err != nil {
		return fmt.Errorf("connect to pinecone index: %w", err)
	}
	defer idxConn.Close()

	vectors := make([]*pinecone.Vector, 0, len(records))
	for i, rec := range records {
		values := make([]float32, len(floats.Embeddings[i]))
		for j, v := range floats.Embeddings[i] {
			values[j] = float32(v)
		}
		schemaJSON, err := json.Marshal(rec.tool.InputSchema)
		if err != nil {
			return fmt.Errorf("marshal schema for %s.%s: %w", rec.service, rec.tool.Name, err)
		}
		metadata, err := structpb.NewStruct(map[string]any{
			"service":     rec.service,
			"tool_name":   rec.tool.Name,
			"description": rec.tool.Description,
			"schema":      string(schemaJSON),
		})
		if err != nil {
			return fmt.Errorf("build metadata for %s.%s: %w", rec.service, rec.tool.Name, err)
		}
		vectors = append(vectors, &pinecone.Vector{
			Id:       rec.service + "." + rec.tool.Name,
			Values:   &values,
			Metadata: metadata,
		})
	}

	count, err := idxConn.UpsertVectors(ctx, vectors)
	if err != nil {
		return fmt.Errorf("upsert vectors: %w", err)
	}
	logger.Info("seeded tools into pinecone", "namespace", toolsNamespace, "upserted", count)
	return nil
}

// embeddingText is what actually gets embedded for one tool — name,
// description, and the JSON input schema, so a semantic search for e.g.
// "refund a customer" or "post to slack" matches on the parameters a
// tool takes, not just its human-written description.
func embeddingText(service string, tool *gomcp.Tool) string {
	schemaJSON, _ := json.Marshal(tool.InputSchema)
	return fmt.Sprintf("service: %s\ntool: %s\ndescription: %s\nparameters: %s",
		service, tool.Name, tool.Description, schemaJSON)
}

func stringPtr(s string) *string { return &s }
