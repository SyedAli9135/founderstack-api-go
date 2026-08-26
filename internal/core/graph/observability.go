package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/core/llm"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// writeAuditLog and writeCostLedgerToolCall/writeCostLedgerLLMCall close
// the "no audit_logs/cost_ledger writer exists anywhere" gap flagged
// when the executor loop first landed. Errors from these are logged by
// the caller, never propagated to abort a run — an audit-trail write
// failing is not itself a reason to fail the founder's actual workflow,
// same reasoning workflow 6's Purge gives for its own best-effort
// external-delete retries.

func writeAuditLog(ctx context.Context, deps RunDeps, state *RunState, action, resourceType string, isError bool) error {
	status := "success"
	if isError {
		status = "error"
	}
	agentID := pgtype.UUID{Bytes: state.AgentID, Valid: true}
	metadata, err := json.Marshal(map[string]any{"run_id": state.WorkflowRunID.String()})
	if err != nil {
		return fmt.Errorf("graph: marshal audit metadata: %w", err)
	}

	return tenant.WithTx(ctx, deps.AppPool, deps.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertAuditLog(ctx, dbgen.InsertAuditLogParams{
			OrgID: deps.OrgID, ActorID: agentID, ActorType: "agent",
			Action: action, ResourceType: &resourceType, Status: &status, MetadataInfo: metadata,
		})
	})
}

func writeCostLedgerToolCall(ctx context.Context, deps RunDeps, state *RunState, toolName string, success bool) error {
	runID := pgtype.UUID{Bytes: state.WorkflowRunID, Valid: true}
	agentID := pgtype.UUID{Bytes: state.AgentID, Valid: true}
	provider := toolName // the qualified "service.tool_name" doubles as a useful provider label for a tool_call row

	return tenant.WithTx(ctx, deps.AppPool, deps.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertCostLedgerEntry(ctx, dbgen.InsertCostLedgerEntryParams{
			OrgID: deps.OrgID, RunID: runID, AgentID: agentID, CostType: "tool_call",
			// Genuinely $0, not a gap like the LLM side used to be: none of
			// the 8 tool servers this codebase calls (Stripe, Slack,
			// GitHub, Notion, LinkedIn, Discord, Google Drive, Google
			// Calendar) charge per-API-call — a per-tool-call cost line
			// only matters if a future integration's API is itself
			// metered, which none of today's 18 tools are.
			Provider: &provider, EstimatedCostUsd: 0,
		})
	})
}

func writeCostLedgerLLMCall(ctx context.Context, deps RunDeps, state *RunState, model string, usage llm.TokenUsage) error {
	runID := pgtype.UUID{Bytes: state.WorkflowRunID, Valid: true}
	agentID := pgtype.UUID{Bytes: state.AgentID, Valid: true}
	inputTokens := int32(usage.InputTokens)
	outputTokens := int32(usage.OutputTokens)
	cachedTokens := int32(usage.CachedTokens)

	return tenant.WithTx(ctx, deps.AppPool, deps.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertCostLedgerEntry(ctx, dbgen.InsertCostLedgerEntryParams{
			OrgID: deps.OrgID, RunID: runID, AgentID: agentID, CostType: "llm_inference",
			Model: &model, InputTokens: &inputTokens, OutputTokens: &outputTokens, CachedTokens: &cachedTokens,
			// llm.EstimateCostUSD is a rough interim estimate (see its own
			// doc comment) — real, billing-grade per-token pricing is
			// still workflow 11's job. This exists so
			// policy_scope.max_cost_per_run_usd has a real number to
			// enforce against instead of a counter that never moves.
			EstimatedCostUsd: llm.EstimateCostUSD(model, usage),
		})
	})
}
