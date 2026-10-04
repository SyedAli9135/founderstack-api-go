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
//
// In process it is exact. With EnableRedis it also relays through Redis, so a
// run executing on one API instance can be watched from a browser connected to
// another — which is the normal case behind a load balancer.
type EventBus struct {
	mu      sync.RWMutex
	subs    map[uuid.UUID][]*subscriber
	mirrors map[uuid.UUID]mirrorTarget
	relay   *relay
}

// subscriber owns its channel's closing, so the channel can be shut by either
// the subscriber's own unsubscribe or the bus's CloseAll without a double close.
type subscriber struct {
	ch   chan Event
	once sync.Once
}

func (s *subscriber) shut() { s.once.Do(func() { close(s.ch) }) }

// mirrorTarget is where (and how) a specialist's own events get echoed —
// see LinkChild.
type mirrorTarget struct {
	ParentRunID uuid.UUID
	Role        string
	// stopRemote ends the Redis subscription that carries the specialist's
	// events when it runs on another instance; nil without a relay.
	stopRemote func()
}

// LinkChild makes every event subsequently published for childRunID also
// get delivered — as a second, tagged copy (SubRunID/AgentRole set,
// RunID rewritten to parentRunID) — to parentRunID's own subscribers.
// Lets a workflow 18 team run's frontend follow every specialist's live
// events through the one SSE connection it already holds on the
// orchestrator's run, instead of opening one stream per specialist.
// Call before dispatching the specialist (graph.RunDeps.A2AClient.Dispatch)
// so no early event is missed; UnlinkChild once dispatch returns.
//
// The specialist's run may execute on a different instance (the dispatch is an
// HTTP call back through the load balancer): its events then arrive over the
// relay and are mirrored here, onto this instance's subscribers and, through
// the relay again, those on every other instance.
func (b *EventBus) LinkChild(childRunID, parentRunID uuid.UUID, role string) {
	stop := b.subscribeRemote(childRunID, func(ev Event) {
		mirrored := mirrorEvent(ev, parentRunID, role)
		b.deliverLocal(parentRunID, mirrored)
		b.relayOut(mirrored)
	})
	b.mu.Lock()
	b.mirrors[childRunID] = mirrorTarget{ParentRunID: parentRunID, Role: role, stopRemote: stop}
	b.mu.Unlock()
}

// UnlinkChild removes a mirror registered by LinkChild — always call this
// once a specialist's dispatch has returned (success or failure), even
// though a finished run stops publishing on its own; otherwise the map
// entry (and its subscriber lookups) would leak for the process lifetime.
func (b *EventBus) UnlinkChild(childRunID uuid.UUID) {
	b.mu.Lock()
	target := b.mirrors[childRunID]
	delete(b.mirrors, childRunID)
	b.mu.Unlock()
	if target.stopRemote != nil {
		target.stopRemote()
	}
}

func mirrorEvent(ev Event, parent uuid.UUID, role string) Event {
	child := ev.RunID
	ev.RunID = parent
	ev.SubRunID = &child
	ev.AgentRole = role
	return ev
}

// NewEventBus builds an empty EventBus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[uuid.UUID][]*subscriber), mirrors: make(map[uuid.UUID]mirrorTarget)}
}

// Subscribe returns a channel that receives every event published for
// runID, and an unsubscribe func the caller must call exactly once (e.g.
// when the SSE client disconnects) to release it. The channel is buffered
// so a slow subscriber never blocks the engine's publish side.
func (b *EventBus) Subscribe(runID uuid.UUID) (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 64)}

	b.mu.Lock()
	b.subs[runID] = append(b.subs[runID], sub)
	b.mu.Unlock()

	// Events from the same run published on other instances.
	stopRemote := b.subscribeRemote(runID, func(ev Event) {
		select {
		case sub.ch <- ev:
		default:
		}
	})

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			// Stop the relay first: it delivers into sub.ch, which is about to close.
			stopRemote()
			b.mu.Lock()
			defer b.mu.Unlock()
			subs := b.subs[runID]
			for i, c := range subs {
				if c == sub {
					b.subs[runID] = append(subs[:i:i], subs[i+1:]...)
					break
				}
			}
			if len(b.subs[runID]) == 0 {
				delete(b.subs, runID)
			}
			sub.shut()
		})
	}
	return sub.ch, unsubscribe
}

// CloseAll closes every subscriber's channel, which ends their SSE streams.
// Called when the server begins shutting down: an open stream otherwise never
// finishes, and http.Server.Shutdown would wait out its whole timeout for it.
func (b *EventBus) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for runID, subs := range b.subs {
		for _, s := range subs {
			s.shut()
		}
		delete(b.subs, runID)
	}
}

// deliverLocal hands ev to this process's subscribers of runID, never blocking.
func (b *EventBus) deliverLocal(runID uuid.UUID, ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs[runID] {
		select {
		case s.ch <- ev:
		default:
		}
	}
}

// Publish delivers ev (Timestamp filled in if zero) to every current
// subscriber of ev.RunID. Never blocks: a subscriber whose buffer is full
// just misses this event rather than stalling the run.
func (b *EventBus) Publish(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	b.deliverLocal(ev.RunID, ev)
	b.relayOut(ev)

	// Mirror a linked specialist's event onto its orchestrator's own
	// subscribers too — see LinkChild. ev.SubRunID is deliberately not
	// already set here (a plain run never sets it), so this only ever
	// fires one level deep: a mirrored copy's own RunID becomes the
	// parent's, which has no mirror entry of its own.
	b.mu.RLock()
	target, ok := b.mirrors[ev.RunID]
	b.mu.RUnlock()
	if ok {
		mirrored := mirrorEvent(ev, target.ParentRunID, target.Role)
		b.deliverLocal(target.ParentRunID, mirrored)
		b.relayOut(mirrored)
	}
}
