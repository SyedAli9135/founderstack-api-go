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
)

// Event is one message published on a run's channel.
type Event struct {
	Type      EventType `json:"type"`
	RunID     uuid.UUID `json:"run_id"`
	Data      any       `json:"data,omitempty"`
	Timestamp time.Time `json:"timestamp"`
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

// ApprovalRequiredData is EventApprovalRequired's Data — added so
// founderstack-web's ApprovalCard can render straight from this SSE event
// (approval id, risk badge, the pending tool-call batch) without a second
// GET /approvals/{id} round trip for the inline live-run case.
type ApprovalRequiredData struct {
	ApprovalID string         `json:"approval_id"`
	RiskLevel  string         `json:"risk_level"`
	ToolCalls  []llm.ToolCall `json:"tool_calls"`
}

// EventBus is a small pub/sub keyed by run_id: node functions (and the
// engine itself) publish, the SSE handler subscribes. Safe for concurrent
// use by multiple runs and multiple subscribers.
type EventBus struct {
	mu   sync.RWMutex
	subs map[uuid.UUID][]chan Event
}

// NewEventBus builds an empty EventBus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[uuid.UUID][]chan Event)}
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
}
