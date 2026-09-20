package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/founderstack/api/internal/core/llm"
)

// TeamMember is one specialist an orchestrator's delegate node can
// dispatch a subtask to — resolved once by whatever launches a team run
// (internal/api/teams) from agent_team_members, then closed over by
// BuildTeamNodes the same way RunDeps closes over every other run
// dependency BuildNodes needs.
type TeamMember struct {
	AgentID     uuid.UUID
	Name        string
	Description string
	Role        string
}

// BuildTeamNodes wires the 4-node graph an orchestrator's own run drives:
// planner (team variant) -> delegate -> validator -> reporter. Reuses
// validatorNode/reporterNode completely unchanged: each specialist's final
// output is appended to state.Conversation as an ordinary tool-role
// message (see delegateNode), so validator's existing prompt-injection
// scan already covers a specialist's (untrusted, LLM-produced) output with
// zero team-specific logic of its own — the same reasoning that already
// applies to any other tool result. delegateNode itself also produces the
// synthesized final summary into state.Output before handing off, so
// reporterNode's job (prepend warnings, write the trace row) is identical
// to the single-agent path too.
func BuildTeamNodes(deps RunDeps, members []TeamMember) Nodes {
	return Nodes{
		"planner":   teamPlannerNode(deps),
		"delegate":  delegateNode(deps, members),
		"validator": validatorNode(deps),
		"reporter":  reporterNode(deps),
	}
}

// teamPlannerNode is plannerNode's only difference from the single-agent
// path: it hands off to "delegate" instead of "executor". Kept as its own
// function (not a plannerNode(deps, nextNode) parameter) so the ordinary,
// by-far-more-common single-agent path stays untouched by workflow 18.
func teamPlannerNode(deps RunDeps) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		start := time.Now()
		state.Conversation = []llm.Message{{Role: llm.RoleUser, Content: state.Input}}
		if err := writeWorkflowStep(ctx, deps, state, "planner", "planning", map[string]any{"input": state.Input}, nil, nil, time.Since(start), "completed"); err != nil {
			slog.Error("graph: write workflow_steps for team planner failed", "run_id", state.WorkflowRunID, "err", err)
		}
		return "delegate", nil
	}
}

// subtask is one line of the orchestrator's own decomposition of the run's
// input — decided by the model itself (see decomposeTask), never a
// harness heuristic, the same "the model reasons, the harness only
// executes" philosophy executorNode's tool-choice loop already follows.
type subtask struct {
	Role string `json:"role"`
	Task string `json:"task"`
}

type decomposePlan struct {
	Subtasks []subtask `json:"subtasks"`
}

// delegateNode decomposes state.Input into one subtask per specialist
// role, dispatches every subtask in parallel via A2A tasks/send
func delegateNode(deps RunDeps, members []TeamMember) NodeFunc {
	return func(ctx context.Context, state *RunState) (NodeName, error) {
		decomposeStart := time.Now()
		plan, err := decomposeTask(ctx, deps, state, members)
		if err != nil {
			return "", fmt.Errorf("graph: decompose team task: %w", err)
		}
		if len(plan) == 0 {
			return "", fmt.Errorf("graph: model produced no subtasks to delegate — the team may need a clearer task or better-described member roles")
		}
		if err := writeWorkflowStep(ctx, deps, state, "delegate", "decompose",
			map[string]any{"input": state.Input}, map[string]any{"subtasks": plan}, nil,
			time.Since(decomposeStart), "completed"); err != nil {
			slog.Error("graph: write workflow_steps for delegate decompose failed", "run_id", state.WorkflowRunID, "err", err)
		}

		g, gctx := errgroup.WithContext(ctx)
		results := make([]llm.Message, len(plan))
		for i, sub := range plan {
			i, sub := i, sub
			member, ok := memberForRole(members, sub.Role)
			if !ok {
				results[i] = llm.Message{Role: llm.RoleTool, Name: "a2a:" + sub.Role, Content: fmt.Sprintf("no team member has role %q — subtask skipped", sub.Role), IsError: true}
				continue
			}
			g.Go(func() error {
				results[i] = dispatchSubtask(gctx, deps, state, member, sub)
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return "", fmt.Errorf("graph: team dispatch: %w", err)
		}
		state.Conversation = append(state.Conversation, results...)

		summary, err := synthesizeFinalOutput(ctx, deps, state)
		if err != nil {
			return "", fmt.Errorf("graph: synthesize team result: %w", err)
		}
		state.Output = summary
		return "validator", nil
	}
}

// dispatchSubtask runs one specialist end to end: links its future events
// onto the orchestrator's own subscribers *before* dispatch (so nothing
// early is missed), publishes EventDelegated immediately (the live feed
// shouldn't wait for the specialist to actually start to show "→
// Delegated to X"), makes the real A2A HTTP call, then records the
// outcome as a tool-role message ready to fold into state.Conversation.
// A specialist's own failure never aborts the team run — same as an
// ordinary failed tool call never aborts executorNode's loop — it
// surfaces as an error-flagged message the synthesis call and validator
// both see.
func dispatchSubtask(ctx context.Context, deps RunDeps, state *RunState, member TeamMember, sub subtask) llm.Message {
	start := time.Now()
	childRunID := uuid.New()
	deps.Engine.Bus.LinkChild(childRunID, state.WorkflowRunID, sub.Role)
	defer deps.Engine.Bus.UnlinkChild(childRunID)

	deps.Engine.Bus.Publish(Event{
		Type: EventDelegated, RunID: state.WorkflowRunID,
		Data: DelegatedData{Role: sub.Role, AgentName: member.Name, SubRunID: childRunID},
	})

	orgID := uuid.UUID(deps.OrgID.Bytes)
	task, dispatchErr := deps.A2AClient.Dispatch(ctx, orgID, state.WorkflowRunID, member.AgentID, childRunID, sub.Task)

	content := task.Text()
	isError := dispatchErr != nil
	if isError && content == "" {
		content = dispatchErr.Error()
	}

	if err := writeWorkflowStep(ctx, deps, state, "delegate", "a2a_dispatch",
		map[string]any{"role": sub.Role, "agent": member.Name, "task": sub.Task},
		map[string]any{"output": content, "is_error": isError}, nil, time.Since(start),
		stepStatusFor(isError)); err != nil {
		slog.Error("graph: write workflow_steps for a2a dispatch failed", "run_id", state.WorkflowRunID, "role", sub.Role, "err", err)
	}
	return llm.Message{Role: llm.RoleTool, Name: "a2a:" + sub.Role, Content: content, IsError: isError}
}

func stepStatusFor(isError bool) string {
	if isError {
		return "failed"
	}
	return "completed"
}

func memberForRole(members []TeamMember, role string) (TeamMember, bool) {
	for _, m := range members {
		if m.Role == role {
			return m, true
		}
	}
	return TeamMember{}, false
}

// decomposeTask asks the orchestrator's own model to split input into one
// subtask per specialist role it chooses to use — plain-text JSON-mode
// prompting, not a tool schema: this is a one-shot structured request with
// no back-and-forth, so it doesn't need executorNode's tool-calling
// machinery. tools is deliberately nil so the model can't get distracted
// trying to call an MCP tool directly instead of delegating.
func decomposeTask(ctx context.Context, deps RunDeps, state *RunState, members []TeamMember) ([]subtask, error) {
	var roles strings.Builder
	for _, m := range members {
		fmt.Fprintf(&roles, "- role %q (%s): %s\n", m.Role, m.Name, m.Description)
	}

	systemPrompt := deps.SystemPrompt + "\n\nYou are the orchestrator of a team of specialist agents. " +
		"Break the founder's request into one subtask per specialist role you need, from this roster:\n" + roles.String() +
		"\nRespond with ONLY a JSON object of the exact shape " +
		`{"subtasks":[{"role":"<role>","task":"<what this specialist should do>"}]}` +
		" — no prose before or after it. Only use roles from the roster above. " +
		"Use every specialist whose expertise the request genuinely needs, and no others."

	resp, err := sendWithRetry(ctx, deps.ChatClient, systemPrompt, []llm.Message{{Role: llm.RoleUser, Content: state.Input}}, nil)
	if err != nil {
		return nil, err
	}
	accumulateUsage(state, deps.Model, resp.Usage)
	if err := writeCostLedgerLLMCall(ctx, deps, state, deps.Model, resp.Usage); err != nil {
		slog.Error("graph: write cost_ledger for team decompose call failed", "run_id", state.WorkflowRunID, "err", err)
	}

	var plan decomposePlan
	if err := json.Unmarshal([]byte(extractJSONObject(resp.Content)), &plan); err != nil {
		return nil, fmt.Errorf("model returned non-JSON decomposition: %w", err)
	}

	valid := make([]subtask, 0, len(plan.Subtasks))
	for _, s := range plan.Subtasks {
		if _, ok := memberForRole(members, s.Role); ok && strings.TrimSpace(s.Task) != "" {
			valid = append(valid, s)
		}
	}
	return valid, nil
}

// synthesizeFinalOutput is the "reporter generates final summary" half of
// workflow 18's user story — one more model call, now over every
// specialist's result (already appended to state.Conversation by
// delegateNode), asking for one cohesive answer to the founder's original
// request rather than a raw dump of N separate outputs.
func synthesizeFinalOutput(ctx context.Context, deps RunDeps, state *RunState) (string, error) {
	messages := append([]llm.Message{}, state.Conversation...)
	messages = append(messages, llm.Message{
		Role: llm.RoleUser,
		Content: "Every specialist above has reported back. Write one cohesive final answer to the original " +
			"request, synthesizing their findings — not a list of who said what.",
	})
	resp, err := sendWithRetry(ctx, deps.ChatClient, deps.SystemPrompt, messages, nil)
	if err != nil {
		return "", err
	}
	accumulateUsage(state, deps.Model, resp.Usage)
	if err := writeCostLedgerLLMCall(ctx, deps, state, deps.Model, resp.Usage); err != nil {
		slog.Error("graph: write cost_ledger for team synthesis call failed", "run_id", state.WorkflowRunID, "err", err)
	}
	return resp.Content, nil
}

// extractJSONObject trims any prose a model wraps its JSON in despite
// being asked not to — takes the substring from the first '{' to the
// last '}', a defensive best-effort, not a real parser.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return s
	}
	return s[start : end+1]
}
