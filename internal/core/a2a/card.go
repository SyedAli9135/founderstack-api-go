package a2a

import "fmt"

// AgentCard is the subset of the A2A spec's agent card this codebase
// publishes for each specialist agent — enough for an orchestrator (ours
// or, eventually, an external one) to decide whether and how to delegate
// a task to this agent, not a byte-for-byte implementation of every
// optional spec field.
type AgentCard struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	URL          string       `json:"url"`
	Version      string       `json:"version"`
	Capabilities Capabilities `json:"capabilities"`
	Skills       []Skill      `json:"skills"`
}

type Capabilities struct {
	Streaming bool `json:"streaming"`
}

// Skill mirrors the spec's per-agent capability listing — built from the
// agent's own policy_scope.allowed_tools (see BuildAgentCard) so the card
// reflects what this agent can actually do, not a hand-maintained list
// that drifts from its real policy_scope the way a second source of truth
// always does eventually.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

// BuildAgentCard assembles agentID's card. Takes plain scalars rather than
// a dbgen row type so this package stays free of any DB dependency —
// internal/api/a2a's handler does the query + policy_scope unmarshal and
// passes allowedTools straight through. url is the full absolute
// tasks/send endpoint (cfg.AppBaseURL + the agent-scoped path), version is
// the agent's own agents.version column — the card's version tracks the
// same config-change counter the rest of the app already uses for an
// agent, not a separately maintained API version.
func BuildAgentCard(name, description, model, url string, allowedTools []string, version int32) AgentCard {
	skills := make([]Skill, 0, len(allowedTools))
	for _, tool := range allowedTools {
		skills = append(skills, Skill{ID: tool, Name: tool, Description: "MCP tool: " + tool, Tags: []string{"mcp"}})
	}
	if description == "" {
		description = "FounderStack specialist agent (" + model + ")"
	}
	return AgentCard{
		Name:         name,
		Description:  description,
		URL:          url,
		Version:      fmt.Sprintf("%d", version),
		Capabilities: Capabilities{Streaming: false},
		Skills:       skills,
	}
}
