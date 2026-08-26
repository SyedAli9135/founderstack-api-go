package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// ResolvedTools is what a run needs to offer tools to the model and
// classify a requested call's risk without a per-call registry lookup —
// built once at run start (ResolveTools) from the agent's
// policy_scope.allowed_tools and the org's connected MCP registry.
type ResolvedTools struct {
	Schemas []llm.ToolSchema

	risk map[string]coremcp.RiskLevel // keyed by "service.tool_name"
}

// RiskLevel returns qualifiedToolName's classification. A tool this run
// never actually resolved (shouldn't happen once CheckToolAllowed has
// already run, but never trust that as the only gate) is treated as
// maximally risky — same fail-closed reasoning as RiskLevelFor's own
// nil-annotations fallback.
func (t ResolvedTools) RiskLevel(qualifiedToolName string) coremcp.RiskLevel {
	if level, ok := t.risk[qualifiedToolName]; ok {
		return level
	}
	return coremcp.RiskWriteDestructiveOrFinancial
}

// ResolveTools builds the tool schema list to offer the model, plus each
// one's classified RiskLevel, from registry's live tool catalog filtered
// down to policy.AllowedTools. Tool names are qualified as
// "service.tool_name", matching registry's own naming convention.
func ResolveTools(ctx context.Context, registry *coremcp.Registry, policy PolicyScope) (ResolvedTools, error) {
	allByService, err := registry.ListTools(ctx)
	if err != nil {
		return ResolvedTools{}, fmt.Errorf("graph: list tools for run: %w", err)
	}

	allowed := make(map[string]bool, len(policy.AllowedTools))
	for _, name := range policy.AllowedTools {
		allowed[name] = true
	}

	out := ResolvedTools{risk: make(map[string]coremcp.RiskLevel)}
	for service, tools := range allByService {
		for _, tool := range tools {
			qualified := service + "." + tool.Name
			if !allowed[qualified] {
				continue
			}
			schemaJSON, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return ResolvedTools{}, fmt.Errorf("graph: marshal schema for %s: %w", qualified, err)
			}
			out.Schemas = append(out.Schemas, llm.ToolSchema{
				Name:        qualified,
				Description: tool.Description,
				InputSchema: schemaJSON,
			})
			out.risk[qualified] = coremcp.RiskLevelFor(tool.Annotations)
		}
	}
	return out, nil
}
