package graph

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NodeFunc is one node in the graph: given the current state, it does its
// work and returns which node runs next (NodeComplete to end the run,
// NodeAwaitingApproval to suspend it). Node implementations (planner_node,
// executor_node, ...) land in later workflow 9 build steps — this package
// only sequences whatever Nodes map it's given.
type NodeFunc func(ctx context.Context, state *RunState) (next NodeName, err error)

// Nodes maps a NodeName to the function implementing it.
type Nodes map[NodeName]NodeFunc

// Engine walks a caller-supplied graph of nodes, checkpointing RunState to
// Postgres as it goes, publishing SSE events on Bus, and supporting mid-run
// cancellation and (via Resume) continuing a run suspended at an approval
// gate.
type Engine struct {
	pool *pgxpool.Pool
	Bus  *EventBus

	mu      sync.Mutex
	cancels map[uuid.UUID]context.CancelFunc
}

// NewEngine builds an Engine against pool, which must be the app_user
// (RLS-scoped) pool — every checkpoint write goes through tenant.WithTx,
// which requires it. A single run always has one concrete org_id, so there
// is never a reason for the engine itself to use the cross-tenant
// app_system pool.
func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{
		pool:    pool,
		Bus:     NewEventBus(),
		cancels: make(map[uuid.UUID]context.CancelFunc),
	}
}

// Run executes state's workflow starting at startNode. Callers must pass a
// context detached from any HTTP request — context.Background(), not
// c.Request.Context() — since Gin cancels a request's context the moment
// its handler returns, which for a 202-Accepted endpoint is almost
// immediately; see internal/api/documents/handler.go for the existing
// precedent this follows.
func (e *Engine) Run(ctx context.Context, nodes Nodes, state *RunState, startNode NodeName) error {
	return e.runFrom(ctx, nodes, state, startNode)
}

// Resume reloads runID's checkpoint, applies resumeData (an approval
// decision), and continues execution from wherever the run suspended — the
// interrupt()/thread_id replacement. Returns an error if the run has no
// resumable checkpoint (never started, or already finished).
func (e *Engine) Resume(ctx context.Context, nodes Nodes, orgID, runID uuid.UUID, resumeData ResumeData) error {
	state, currentNode, err := loadCheckpoint(ctx, e.pool, orgID, runID)
	if err != nil {
		return err
	}
	if currentNode == "" || currentNode == NodeComplete {
		return fmt.Errorf("graph: run %s has no resumable checkpoint (current_node empty)", runID)
	}
	state.Approval = &ApprovalResult{Approved: resumeData.Approved, Reason: resumeData.Reason}

	return e.runFrom(ctx, nodes, state, currentNode)
}

// Cancel stops runID's execution at its next ctx.Err() check. Returns false
// if runID has no in-flight goroutine on this process — this map is
// in-memory and per-process, not a cross-instance guarantee.
func (e *Engine) Cancel(runID uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	cancel, ok := e.cancels[runID]
	if !ok {
		return false
	}
	cancel()
	return true
}

// Checkpoint lets a node (or, once executor_node exists, its inner
// tool-calling loop) persist state mid-node — e.g. after every individual
// tool call — not just at the node-transition boundaries runFrom itself
// guarantees. Status is always "running": a mid-node checkpoint is never
// itself a terminal state.
func (e *Engine) Checkpoint(ctx context.Context, state *RunState, currentNode NodeName) error {
	return checkpoint(ctx, e.pool, state, currentNode, "running")
}

func (e *Engine) runFrom(ctx context.Context, nodes Nodes, state *RunState, node NodeName) error {
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancels[state.WorkflowRunID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, state.WorkflowRunID)
		e.mu.Unlock()
		cancel()
	}()

	for {
		if err := ctx.Err(); err != nil {
			// The checkpoint write itself must not inherit ctx's
			// cancellation, or it would fail immediately — same reasoning
			// pgx applies to any post-cancellation cleanup query.
			_ = checkpoint(context.WithoutCancel(ctx), e.pool, state, node, "cancelled")
			e.Bus.Publish(Event{Type: EventError, RunID: state.WorkflowRunID, Data: err.Error()})
			return err
		}

		fn, ok := nodes[node]
		if !ok {
			return fmt.Errorf("graph: no node registered for %q", node)
		}

		e.Bus.Publish(Event{Type: EventNodeStart, RunID: state.WorkflowRunID, Data: NodeTransitionData{Node: string(node), AgentName: state.AgentName}})
		next, err := fn(ctx, state)
		if err != nil {
			// A node blocked on ctx.Done() (e.g. an in-flight tool call)
			// surfaces cancellation as its own returned error here, not via
			// the top-of-loop ctx.Err() check above — classify by ctx.Err()
			// at the time of return, not by whether err happens to be
			// exactly context.Canceled, so a node that wraps it still counts.
			status := "failed"
			if ctx.Err() != nil {
				status = "cancelled"
			}
			_ = checkpoint(context.WithoutCancel(ctx), e.pool, state, node, status)
			e.Bus.Publish(Event{Type: EventError, RunID: state.WorkflowRunID, Data: err.Error()})
			return err
		}
		e.Bus.Publish(Event{Type: EventNodeEnd, RunID: state.WorkflowRunID, Data: NodeTransitionData{Node: string(node), AgentName: state.AgentName}})

		switch next {
		case NodeComplete:
			if err := checkpoint(ctx, e.pool, state, next, "completed"); err != nil {
				return err
			}
			e.Bus.Publish(Event{Type: EventComplete, RunID: state.WorkflowRunID, Data: CompleteData{
				Output: state.Output, TokenUsage: state.TokenUsage, CostSoFarUSD: state.CostSoFarUSD,
			}})
			return nil
		case NodeAwaitingApproval:
			if err := checkpoint(ctx, e.pool, state, next, "awaiting_approval"); err != nil {
				return err
			}
			e.Bus.Publish(Event{Type: EventApprovalRequired, RunID: state.WorkflowRunID})
			return nil
		default:
			if err := checkpoint(ctx, e.pool, state, next, "running"); err != nil {
				return err
			}
			node = next
		}
	}
}
