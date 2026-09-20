package graph

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/founderstack/api/internal/core/llm"
)

// EventType is the SSE event name forwarded by GET /runs/{run_id}/stream.
type EventType string

const (
	EventNodeStart        EventType = "node_start"
	EventNodeEnd          EventType = "node_end"
	EventReasoning        EventType = "reasoning"
	EventToolCall         EventType = "tool_call"
	EventToolResult       EventType = "tool_result"
	EventApprovalRequired EventType = "approval_required"
	EventError            EventType = "error"
	EventToken            EventType = "token"
	EventComplete         EventType = "complete"
	EventIntegrationError EventType = "integration_error"
	EventDelegated        EventType = "delegated"
)

// Event is one message published on a run's channel. SubRunID/AgentRole
// are only ever set on a *mirrored* copy of a specialist's event — see
// EventBus.LinkChild — never on an event published directly for its own
// run_id, so a plain (non-team) run's stream is byte-for-byte unchanged.
type Event struct {
	Type      EventType  `json:"type"`
	RunID     uuid.UUID  `json:"run_id"`
	Data      any        `json:"data,omitempty"`
	Timestamp time.Time  `json:"timestamp"`
	SubRunID  *uuid.UUID `json:"sub_run_id,omitempty"`
	AgentRole string     `json:"agent_role,omitempty"`
}

// DelegatedData is EventDelegated's Data — published on the orchestrator's
// own run the moment a subtask is dispatched, before the specialist's own
// events start arriving (mirrored, see LinkChild) — lets the live feed
// render "→ Delegated to Finance Agent" without waiting on the specialist
// to actually start.
type DelegatedData struct {
	Role      string    `json:"role"`
	AgentName string    `json:"agent_name"`
	SubRunID  uuid.UUID `json:"sub_run_id"`
}

// NodeTransitionData is EventNodeStart/EventNodeEnd's Data
type NodeTransitionData struct {
	Node      string `json:"node"`
	AgentName string `json:"agent_name"`
}

// CompleteData is EventComplete's Data
type CompleteData struct {
	Output       string     `json:"output"`
	TokenUsage   TokenUsage `json:"token_usage"`
	CostSoFarUSD float64    `json:"cost_so_far_usd"`
}

// ApprovalRequiredData is EventApprovalRequired's Data — lets the frontend
// render an approval card straight from the stream, no extra GET needed.
type ApprovalRequiredData struct {
	ApprovalID string         `json:"approval_id"`
	RiskLevel  string         `json:"risk_level"`
	ToolCalls  []llm.ToolCall `json:"tool_calls"`
}

// IntegrationErrorData is EventIntegrationError's Data — published
// alongside (not instead of) the normal EventToolResult for a tool call
// that failed because the org's connection to Service is missing/expired/
// revoked. A distinct event so the live feed can render an actionable
// "reconnect" banner instead of a generic tool-error line.
type IntegrationErrorData struct {
	Service      string `json:"service"`
	ReconnectURL string `json:"reconnect_url"`
}

// EventBus is a small pub/sub keyed by run_id: node functions (and the
// engine itself) publish, the SSE handler subscribes. Safe for concurrent
// use by multiple runs and multiple subscribers.
type EventBus struct {
	mu      sync.RWMutex
	subs    map[uuid.UUID][]chan Event
	mirrors map[uuid.UUID]mirrorTarget
}

// mirrorTarget is where (and how) a specialist's own events get echoed —
// see LinkChild.
type mirrorTarget struct {
	ParentRunID uuid.UUID
	Role        string
}

// LinkChild makes every event subsequently published for childRunID also
// get delivered — as a second, tagged copy (SubRunID/AgentRole set,
// RunID rewritten to parentRunID) — to parentRunID's own subscribers.
// Lets a workflow 18 team run's frontend follow every specialist's live
// events through the one SSE connection it already holds on the
// orchestrator's run, instead of opening one stream per specialist.
// Call before dispatching the specialist (graph.RunDeps.A2AClient.Dispatch)
// so no early event is missed; UnlinkChild once dispatch returns.
func (b *EventBus) LinkChild(childRunID, parentRunID uuid.UUID, role string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mirrors[childRunID] = mirrorTarget{ParentRunID: parentRunID, Role: role}
}

// UnlinkChild removes a mirror registered by LinkChild — always call this
// once a specialist's dispatch has returned (success or failure), even
// though a finished run stops publishing on its own; otherwise the map
// entry (and its subscriber lookups) would leak for the process lifetime.
func (b *EventBus) UnlinkChild(childRunID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.mirrors, childRunID)
}

// NewEventBus builds an empty EventBus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[uuid.UUID][]chan Event), mirrors: make(map[uuid.UUID]mirrorTarget)}
}

// Subscribe returns a channel that receives every event published for
// runID, and an unsubscribe func the caller must call exactly once (e.g.
// when the SSE client disconnects) to release it. The channel is buffered
// so a slow subscriber never blocks the engine's publish side.
func (b *EventBus) Subscribe(runID uuid.UUID) (<-chan Event, func()) {
	ch := make(chan Event, 64)

	b.mu.Lock()
	b.subs[runID] = append(b.subs[runID], ch)
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			subs := b.subs[runID]
			for i, c := range subs {
				if c == ch {
					b.subs[runID] = append(subs[:i:i], subs[i+1:]...)
					break
				}
			}
			if len(b.subs[runID]) == 0 {
				delete(b.subs, runID)
			}
			close(ch)
		})
	}
	return ch, unsubscribe
}

// Publish delivers ev (Timestamp filled in if zero) to every current
// subscriber of ev.RunID. Never blocks: a subscriber whose buffer is full
// just misses this event rather than stalling the run.
func (b *EventBus) Publish(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs[ev.RunID] {
		select {
		case ch <- ev:
		default:
		}
	}

	// Mirror a linked specialist's event onto its orchestrator's own
	// subscribers too — see LinkChild. ev.SubRunID is deliberately not
	// already set here (a plain run never sets it), so this only ever
	// fires one level deep: a mirrored copy's own RunID becomes the
	// parent's, which has no mirror entry of its own.
	if target, ok := b.mirrors[ev.RunID]; ok {
		mirrored := ev
		mirrored.RunID = target.ParentRunID
		childRunID := ev.RunID
		mirrored.SubRunID = &childRunID
		mirrored.AgentRole = target.Role
		for _, ch := range b.subs[target.ParentRunID] {
			select {
			case ch <- mirrored:
			default:
			}
		}
	}
}
