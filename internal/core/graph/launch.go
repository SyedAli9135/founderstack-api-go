package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/a2a"
	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// PreflightError is a fast-failing reason Launch refuses to start a run at all
type PreflightError struct {
	Code    string
	Message string
}

func (e *PreflightError) Error() string { return e.Message }

var (
	ErrAgentsPaused = &PreflightError{Code: "AGENTS_PAUSED", Message: "agents are paused for this organization"}
	ErrNoBYOKKey    = &PreflightError{Code: "NO_BYOK_KEY", Message: "organization has no active LLM provider key configured"}
)

// ChatClientResolver matches the signature llm.ResolveChatClient itself satisfies.
type ChatClientResolver func(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider llm.ProviderID, model string) (llm.ChatClient, error)

// Launcher resolves everything one run needs (agent config, BYOK chat
// client, tool catalog) and starts it.
type Launcher struct {
	engine            *Engine
	appPool           *pgxpool.Pool
	encryptionKey     []byte
	registry          *coremcp.Registry
	gateway           *coremcp.Gateway
	resolveChatClient ChatClientResolver
	notifier          *notify.Notifier
	a2aClient         *a2a.Client
}

// SetA2AClient wires the Launcher for workflow 18 team runs — LaunchTeam
// panics if called before this (see its own doc comment); every other
// Launcher method is unaffected by whether it's been called.
func (l *Launcher) SetA2AClient(c *a2a.Client) {
	l.a2aClient = c
}

// NewLauncher builds a Launcher against llm.ResolveChatClient. appPool
// must be the app_user (RLS-enforced) pool — every DB op goes through
// tenant.WithTx. notifier may be nil (RunDeps.Notifier is nil-checked).
func NewLauncher(engine *Engine, appPool *pgxpool.Pool, encryptionKey []byte, registry *coremcp.Registry, gateway *coremcp.Gateway, notifier *notify.Notifier) *Launcher {
	return NewLauncherWithResolver(engine, appPool, encryptionKey, registry, gateway, notifier, llm.ResolveChatClient)
}

// NewLauncherWithResolver is NewLauncher with an injectable resolver —
// tests use it to substitute a fake returning llm.MockChatClient.
func NewLauncherWithResolver(engine *Engine, appPool *pgxpool.Pool, encryptionKey []byte, registry *coremcp.Registry, gateway *coremcp.Gateway, notifier *notify.Notifier, resolveChatClient ChatClientResolver) *Launcher {
	return &Launcher{engine: engine, appPool: appPool, encryptionKey: encryptionKey, registry: registry, gateway: gateway, notifier: notifier, resolveChatClient: resolveChatClient}
}

// Preflight checks the org kill switch and BYOK key presence before a
// run is queued — called synchronously, before the workflow_runs INSERT.
func (l *Launcher) Preflight(ctx context.Context, orgID pgtype.UUID) error {
	var settings dbgen.GetOrgRunSettingsRow
	err := tenant.WithTx(ctx, l.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		settings, err = q.GetOrgRunSettings(ctx, orgID)
		return err
	})
	if err != nil {
		return fmt.Errorf("graph: preflight org lookup: %w", err)
	}
	if settings.AgentsPaused {
		return ErrAgentsPaused
	}
	if settings.LlmProvider == nil || *settings.LlmProvider == "" {
		return ErrNoBYOKKey
	}

	var hasKey bool
	err = tenant.WithTx(ctx, l.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.GetActiveKeyByOrgIDAndProvider(ctx, dbgen.GetActiveKeyByOrgIDAndProviderParams{
			OrgID: orgID, Provider: *settings.LlmProvider,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		hasKey = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("graph: preflight key lookup: %w", err)
	}
	if !hasKey {
		return ErrNoBYOKKey
	}
	return nil
}

// Launch resolves RunDeps and runs the workflow in a detached goroutine
// (context.Background(), never the caller's request context).
func (l *Launcher) Launch(orgID, agentID, workflowID, runID uuid.UUID, input string) {
	go func() {
		ctx := context.Background()
		orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
		runPg := pgtype.UUID{Bytes: runID, Valid: true}

		if err := l.run(ctx, orgID, agentID, runID, input); err != nil {
			if status, statusErr := getRunStatus(ctx, l.appPool, orgPg, runPg); statusErr == nil && status == "pending" {
				_ = markRunFailedNoCheckpoint(ctx, l.appPool, orgPg, runPg)
			}
		}
	}()
}

func (l *Launcher) run(ctx context.Context, orgID, agentID, runID uuid.UUID, input string) error {
	orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
	agentPg := pgtype.UUID{Bytes: agentID, Valid: true}
	runPg := pgtype.UUID{Bytes: runID, Valid: true}

	agentRow, nodes, err := l.buildNodesForRun(ctx, orgPg, agentPg)
	if err != nil {
		return err
	}
	state := &RunState{OrgID: orgID, AgentID: agentID, AgentName: agentRow.Name, WorkflowRunID: runID, Input: input}

	if err := markRunStarted(ctx, l.appPool, orgPg, runPg); err != nil {
		return fmt.Errorf("graph: mark run started: %w", err)
	}

	runErr := l.engine.Run(ctx, nodes, state, "planner")

	if err := finalizeIfTerminal(ctx, l.appPool, orgPg, runPg, state); err != nil {
		slog.Error("graph: finalize run failed", "run_id", runID, "engine_err", runErr, "finalize_err", err)
	}
	return runErr
}

// Resume continues a run suspended at approval_gate — same
// resolve-then-drive shape as Launch, but via Engine.Resume.
func (l *Launcher) Resume(orgID, runID uuid.UUID, approved bool, reason string) {
	go func() {
		ctx := context.Background()
		orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
		runPg := pgtype.UUID{Bytes: runID, Valid: true}

		if err := l.resume(ctx, orgID, runID, approved, reason); err != nil {
			slog.Error("graph: resume run failed", "run_id", runID, "err", err)
			if status, statusErr := getRunStatus(ctx, l.appPool, orgPg, runPg); statusErr == nil && status == "awaiting_approval" {
				_ = markRunFailedNoCheckpoint(ctx, l.appPool, orgPg, runPg)
			}
		}
	}()
}

func (l *Launcher) resume(ctx context.Context, orgID, runID uuid.UUID, approved bool, reason string) error {
	orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
	runPg := pgtype.UUID{Bytes: runID, Valid: true}

	var agentPg pgtype.UUID
	if err := tenant.WithTx(ctx, l.appPool, orgPg, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		agentPg, err = q.GetRunAgentID(ctx, dbgen.GetRunAgentIDParams{OrgID: orgPg, ID: runPg})
		return err
	}); err != nil {
		return fmt.Errorf("graph: resolve run's agent: %w", err)
	}

	_, nodes, err := l.buildNodesForRun(ctx, orgPg, agentPg)
	if err != nil {
		return err
	}

	resumeErr := l.engine.Resume(ctx, nodes, orgID, runID, ResumeData{Approved: approved, Reason: reason})

	if finalState, _, loadErr := loadCheckpoint(ctx, l.appPool, orgID, runID); loadErr == nil {
		if err := finalizeIfTerminal(ctx, l.appPool, orgPg, runPg, finalState); err != nil {
			slog.Error("graph: finalize resumed run failed", "run_id", runID, "resume_err", resumeErr, "finalize_err", err)
		}
	} else {
		slog.Error("graph: reload checkpoint after resume failed", "run_id", runID, "err", loadErr)
	}
	return resumeErr
}

// buildNodesForRun resolves the agent config, chat client, and tool
// catalog shared by run() and resume() — kept in lockstep so a resumed
// run sees the exact same tool/policy/model config it started with.
func (l *Launcher) buildNodesForRun(ctx context.Context, orgPg, agentPg pgtype.UUID) (dbgen.GetAgentRow, Nodes, error) {
	agentRow, deps, err := l.buildRunDeps(ctx, orgPg, agentPg)
	if err != nil {
		return dbgen.GetAgentRow{}, nil, err
	}
	return agentRow, BuildNodes(deps), nil
}

// buildRunDeps is buildNodesForRun's dependency-resolution half, split out
// so runTeam/RunSpecialist (workflow 18) can wire the *same* resolved
// RunDeps into BuildTeamNodes/BuildNodes respectively instead of
// duplicating this resolution logic — chat client, tool catalog, and
// policy_scope parsing must stay identical across every run shape.
func (l *Launcher) buildRunDeps(ctx context.Context, orgPg, agentPg pgtype.UUID) (dbgen.GetAgentRow, RunDeps, error) {
	var agentRow dbgen.GetAgentRow
	var settings dbgen.GetOrgRunSettingsRow
	err := tenant.WithTx(ctx, l.appPool, orgPg, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		agentRow, err = q.GetAgent(ctx, dbgen.GetAgentParams{OrgID: orgPg, ID: agentPg})
		if err != nil {
			return fmt.Errorf("fetch agent: %w", err)
		}
		settings, err = q.GetOrgRunSettings(ctx, orgPg)
		if err != nil {
			return fmt.Errorf("fetch org settings: %w", err)
		}
		return nil
	})
	if err != nil {
		return dbgen.GetAgentRow{}, RunDeps{}, fmt.Errorf("graph: %w", err)
	}
	if settings.LlmProvider == nil || *settings.LlmProvider == "" {
		return dbgen.GetAgentRow{}, RunDeps{}, fmt.Errorf("graph: %w", llm.ErrNoActiveKey)
	}
	if agentRow.Model == nil || *agentRow.Model == "" {
		return dbgen.GetAgentRow{}, RunDeps{}, errors.New("graph: agent has no model configured")
	}

	var policy PolicyScope
	if err := json.Unmarshal(agentRow.PolicyScope, &policy); err != nil {
		return dbgen.GetAgentRow{}, RunDeps{}, fmt.Errorf("graph: unmarshal policy_scope: %w", err)
	}

	chatClient, err := l.resolveChatClient(ctx, l.appPool, l.encryptionKey, orgPg, llm.ProviderID(*settings.LlmProvider), *agentRow.Model)
	if err != nil {
		return dbgen.GetAgentRow{}, RunDeps{}, fmt.Errorf("graph: resolve chat client: %w", err)
	}

	tools, err := ResolveTools(ctx, l.registry, policy)
	if err != nil {
		return dbgen.GetAgentRow{}, RunDeps{}, fmt.Errorf("graph: resolve tools: %w", err)
	}

	deps := RunDeps{
		Engine: l.engine, ChatClient: chatClient, Gateway: l.gateway,
		Tools: tools, Policy: policy, SystemPrompt: agentRow.SystemPrompt, OrgID: orgPg,
		AppPool: l.appPool, Model: *agentRow.Model, Notifier: l.notifier,
	}
	return agentRow, deps, nil
}

// markRunFailedNoCheckpoint handles the pre-Engine.Run failure path: no
// checkpoint exists yet to classify a status from, so this writes
// 'failed' directly rather than through checkpoint()'s NodeName machinery.
func markRunFailedNoCheckpoint(ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID) error {
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.MarkRunFailedPreflight(ctx, dbgen.MarkRunFailedPreflightParams{OrgID: orgID, ID: runID})
	})
}

// --- Multi-Agent Team Run (A2A) ---

// LaunchTeam is Launch's team-orchestrator counterpart: same
// resolve-then-detached-goroutine shape, but wires BuildTeamNodes instead
// of BuildNodes and requires SetA2AClient to have been called first
// (panics otherwise — a misconfigured deployment should fail loudly at
// the one call site that needs A2A_TASK_TOKEN_SECRET, not silently run a
// team with every specialist dispatch failing).
func (l *Launcher) LaunchTeam(orgID, orchestratorAgentID, runID uuid.UUID, members []TeamMember, input string) {
	if l.a2aClient == nil {
		panic("graph: LaunchTeam called before SetA2AClient — see cmd/api/main.go wiring")
	}
	go func() {
		ctx := context.Background()
		orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
		runPg := pgtype.UUID{Bytes: runID, Valid: true}

		if err := l.runTeam(ctx, orgID, orchestratorAgentID, runID, members, input); err != nil {
			if status, statusErr := getRunStatus(ctx, l.appPool, orgPg, runPg); statusErr == nil && status == "pending" {
				_ = markRunFailedNoCheckpoint(ctx, l.appPool, orgPg, runPg)
			}
		}
	}()
}

func (l *Launcher) runTeam(ctx context.Context, orgID, agentID, runID uuid.UUID, members []TeamMember, input string) error {
	orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
	agentPg := pgtype.UUID{Bytes: agentID, Valid: true}
	runPg := pgtype.UUID{Bytes: runID, Valid: true}

	agentRow, deps, err := l.buildRunDeps(ctx, orgPg, agentPg)
	if err != nil {
		return err
	}
	deps.A2AClient = l.a2aClient
	nodes := BuildTeamNodes(deps, members)
	state := &RunState{OrgID: orgID, AgentID: agentID, AgentName: agentRow.Name, WorkflowRunID: runID, Input: input}

	if err := markRunStarted(ctx, l.appPool, orgPg, runPg); err != nil {
		return fmt.Errorf("graph: mark team run started: %w", err)
	}

	runErr := l.engine.Run(ctx, nodes, state, "planner")

	if err := finalizeIfTerminal(ctx, l.appPool, orgPg, runPg, state); err != nil {
		slog.Error("graph: finalize team run failed", "run_id", runID, "engine_err", runErr, "finalize_err", err)
	}
	return runErr
}

// RunSpecialist executes one specialist's delegated sub-run and returns
// its final output — called synchronously by internal/api/a2a's real
// tasks/send HTTP handler (itself invoked by the orchestrator's own
// delegate node — see internal/core/graph/team_nodes.go and
// internal/core/a2a/client.go), unlike Launch/LaunchTeam/Resume, which all
// background themselves for a caller that must return immediately (a
// 202-accepted HTTP response). ctx is deliberately the caller's own
// context here, not context.Background(): the a2a HTTP handler blocks on
// this call for its entire duration, so canceling the orchestrator's run
// (which cancels its in-flight HTTP calls to specialists) correctly
// propagates all the way down to this specialist's own engine.Run too.
func (l *Launcher) RunSpecialist(ctx context.Context, orgID, agentID, runID uuid.UUID, input string) (output string, err error) {
	orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
	agentPg := pgtype.UUID{Bytes: agentID, Valid: true}
	runPg := pgtype.UUID{Bytes: runID, Valid: true}

	agentRow, deps, err := l.buildRunDeps(ctx, orgPg, agentPg)
	if err != nil {
		return "", err
	}
	nodes := BuildNodes(deps)
	state := &RunState{OrgID: orgID, AgentID: agentID, AgentName: agentRow.Name, WorkflowRunID: runID, Input: input}

	if err := markRunStarted(ctx, l.appPool, orgPg, runPg); err != nil {
		return "", fmt.Errorf("graph: mark specialist run started: %w", err)
	}

	runErr := l.engine.Run(ctx, nodes, state, "planner")

	if err := finalizeIfTerminal(context.WithoutCancel(ctx), l.appPool, orgPg, runPg, state); err != nil {
		slog.Error("graph: finalize specialist run failed", "run_id", runID, "engine_err", runErr, "finalize_err", err)
	}
	return state.Output, runErr
}
