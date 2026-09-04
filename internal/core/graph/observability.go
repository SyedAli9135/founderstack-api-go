package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/core/llm"
	"github.com/founderstack/api/internal/core/pii"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

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
			EstimatedCostUsd: llm.EstimateCostUSD(model, usage),
		})
	})
}

// writeWorkflowStep persists one row of the run's trace —
// input/output are PII-sanitized before storage since a founder's browser
// is the audience. Best-effort like the cost_ledger/audit_log writers
// above: a trace gap shouldn't fail the run itself.
func writeWorkflowStep(ctx context.Context, deps RunDeps, state *RunState, nodeName, stepType string, input, output any, tokens *llm.TokenUsage, duration time.Duration, status string) error {
	runID := pgtype.UUID{Bytes: state.WorkflowRunID, Valid: true}
	var agentName *string
	if state.AgentName != "" {
		agentName = &state.AgentName
	}

	inputData, err := marshalSanitized(input)
	if err != nil {
		return fmt.Errorf("graph: marshal step input: %w", err)
	}
	outputData, err := marshalSanitized(output)
	if err != nil {
		return fmt.Errorf("graph: marshal step output: %w", err)
	}

	durationMs := int32(duration.Milliseconds())
	var inputTokens, outputTokens *int32
	if tokens != nil {
		it, ot := int32(tokens.InputTokens), int32(tokens.OutputTokens)
		inputTokens, outputTokens = &it, &ot
	}

	return tenant.WithTx(ctx, deps.AppPool, deps.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.InsertWorkflowStep(ctx, dbgen.InsertWorkflowStepParams{
			RunID: runID, NodeName: nodeName, StepType: stepType, AgentName: agentName,
			InputData: inputData, OutputData: outputData,
			InputTokens: inputTokens, OutputTokens: outputTokens,
			DurationMs: &durationMs, Status: &status,
		})
	})
}

// marshalSanitized JSON-encodes v (nil-safe — a nil v stores no row data)
// then redacts it via pii.SanitizeJSON before it's written.
func marshalSanitized(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return pii.SanitizeJSON(raw), nil
}
