package sop

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	cron "github.com/robfig/cron/v3"
)

// Mirrors internal/api/agents' own limits and defaults, so a SOP can never
// produce an agent the agents API itself would have rejected.
const (
	DefaultAgentType       = "specialist"
	DefaultModel           = "claude-sonnet-5"
	DefaultMaxOutputTokens = int32(4096)
	DefaultTemperature     = 0.3
	MinSystemPromptLen     = 50
	maxNameLen             = 255

	// A playbook is deployed into every client workspace and its prompt is
	// sent to the model on every run, so each piece is bounded.
	maxSystemPromptLen = 20000
	maxTemplateLen     = 20000
	maxParameters      = 50
	maxParamValueLen   = 2000
	maxAllowedTools    = 200
)

var validTriggerTypes = map[string]bool{"manual": true, "scheduled": true, "webhook": true}

type PolicyScope struct {
	MaxToolCalls     *int32   `json:"max_tool_calls,omitempty"`
	MaxCostPerRunUSD *float64 `json:"max_cost_per_run_usd,omitempty"`
	AllowedTools     []string `json:"allowed_tools"`
}

type AgentConfig struct {
	Name            string      `json:"name"`
	Description     *string     `json:"description,omitempty"`
	AgentType       string      `json:"agent_type,omitempty"`
	Model           string      `json:"model,omitempty"`
	SystemPrompt    string      `json:"system_prompt"`
	MaxOutputTokens *int32      `json:"max_output_tokens,omitempty"`
	Temperature     *float64    `json:"temperature,omitempty"`
	PolicyScope     PolicyScope `json:"policy_scope"`
}

type WorkflowConfig struct {
	Name                   string  `json:"name"`
	Description            *string `json:"description,omitempty"`
	TriggerType            string  `json:"trigger_type"`
	CronExpression         *string `json:"cron_expression,omitempty"`
	RequiresApproval       bool    `json:"requires_approval"`
	TaskInputTemplate      *string `json:"task_input_template,omitempty"`
	EstimatedManualMinutes *int32  `json:"estimated_manual_minutes,omitempty"`
}

// Parameter is a named {{key}} placeholder a SOP's system prompt or task
// input can use; Default applies wherever a deployment doesn't override it.
type Parameter struct {
	Key     string `json:"key"`
	Label   string `json:"label,omitempty"`
	Default string `json:"default"`
}

// Spec is one version of a playbook: everything a deployment is rendered from.
type Spec struct {
	Agent      AgentConfig     `json:"agent_config"`
	Workflow   *WorkflowConfig `json:"workflow_config,omitempty"`
	Parameters []Parameter     `json:"parameters"`
}

// Overrides are a single client's deviations from the playbook. They're
// stored on the deployment and re-applied on every sync, so they survive
// version updates; a parameter the new version no longer declares is
// simply ignored rather than failing the sync.
type Overrides struct {
	Params           map[string]string `json:"params,omitempty"`
	MaxCostPerRunUSD *float64          `json:"max_cost_per_run_usd,omitempty"`
	MaxToolCalls     *int32            `json:"max_tool_calls,omitempty"`
	CronExpression   *string           `json:"cron_expression,omitempty"`
	RequiresApproval *bool             `json:"requires_approval,omitempty"`
}

// Error is a validation failure with a stable code the API returns as-is.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

var (
	paramKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	placeholderPattern = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)
)

// Validate checks a spec against the same rules the agents and workflows
// APIs enforce, plus that every {{placeholder}} it uses is declared.
// knownTools is the full "service.tool" catalog from the MCP registry.
func Validate(s Spec, knownTools map[string]bool) error {
	a := s.Agent
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > maxNameLen {
		return invalid("INVALID_AGENT_NAME", "agent_config.name is required (max %d characters)", maxNameLen)
	}
	if len(strings.TrimSpace(a.SystemPrompt)) < MinSystemPromptLen {
		return invalid("SYSTEM_PROMPT_TOO_SHORT", "agent_config.system_prompt must be at least %d characters", MinSystemPromptLen)
	}
	if len(a.SystemPrompt) > maxSystemPromptLen {
		return invalid("SYSTEM_PROMPT_TOO_LONG", "agent_config.system_prompt can be at most %d characters", maxSystemPromptLen)
	}
	if len(a.PolicyScope.AllowedTools) > maxAllowedTools {
		return invalid("TOO_MANY_TOOLS", "agent_config.policy_scope.allowed_tools can list at most %d tools", maxAllowedTools)
	}
	if err := validatePolicyScope(a.PolicyScope, knownTools); err != nil {
		return err
	}
	if len(s.Parameters) > maxParameters {
		return invalid("TOO_MANY_PARAMETERS", "a SOP can declare at most %d parameters", maxParameters)
	}

	declared := map[string]bool{}
	for _, p := range s.Parameters {
		if len(p.Default) > maxParamValueLen {
			return invalid("PARAMETER_TOO_LONG", "parameter %q default can be at most %d characters", p.Key, maxParamValueLen)
		}
		if !paramKeyPattern.MatchString(p.Key) {
			return invalid("INVALID_PARAMETER_KEY", "parameter key %q must be lowercase letters, digits and underscores, starting with a letter", p.Key)
		}
		if declared[p.Key] {
			return invalid("DUPLICATE_PARAMETER_KEY", "parameter %q is declared twice", p.Key)
		}
		declared[p.Key] = true
	}
	texts := []string{a.SystemPrompt}

	if w := s.Workflow; w != nil {
		if strings.TrimSpace(w.Name) == "" || len(w.Name) > maxNameLen {
			return invalid("INVALID_WORKFLOW_NAME", "workflow_config.name is required (max %d characters)", maxNameLen)
		}
		if !validTriggerTypes[w.TriggerType] {
			return invalid("UNKNOWN_TRIGGER_TYPE", "workflow_config.trigger_type must be manual, scheduled, or webhook")
		}
		if w.TriggerType == "scheduled" {
			if err := validateCron(w.CronExpression); err != nil {
				return err
			}
		}
		if w.EstimatedManualMinutes != nil && *w.EstimatedManualMinutes < 0 {
			return invalid("INVALID_MANUAL_MINUTES", "workflow_config.estimated_manual_minutes can't be negative")
		}
		if w.TaskInputTemplate != nil {
			if len(*w.TaskInputTemplate) > maxTemplateLen {
				return invalid("TASK_TEMPLATE_TOO_LONG", "workflow_config.task_input_template can be at most %d characters", maxTemplateLen)
			}
			texts = append(texts, *w.TaskInputTemplate)
		}
	}

	for _, t := range texts {
		for _, key := range Placeholders(t) {
			if !declared[key] {
				return invalid("UNDECLARED_PARAMETER", "{{%s}} is used but not declared as a parameter", key)
			}
		}
	}
	return nil
}

// ValidateOverrides checks one client's overrides against the spec they'll
// be applied to.
func ValidateOverrides(s Spec, o Overrides) error {
	declared := map[string]bool{}
	for _, p := range s.Parameters {
		declared[p.Key] = true
	}
	for key, value := range o.Params {
		if !declared[key] {
			return invalid("UNKNOWN_PARAMETER", "%q is not a parameter of this SOP", key)
		}
		if len(value) > maxParamValueLen {
			return invalid("PARAMETER_TOO_LONG", "parameter %q can be at most %d characters", key, maxParamValueLen)
		}
	}
	if o.MaxCostPerRunUSD != nil && *o.MaxCostPerRunUSD <= 0 {
		return invalid("INVALID_COST_CAP", "max_cost_per_run_usd must be a positive number")
	}
	if o.MaxToolCalls != nil && *o.MaxToolCalls <= 0 {
		return invalid("INVALID_TOOL_CALL_CAP", "max_tool_calls must be a positive number")
	}
	if o.CronExpression != nil {
		if s.Workflow == nil || s.Workflow.TriggerType != "scheduled" {
			return invalid("SCHEDULE_NOT_APPLICABLE", "cron_expression can only be overridden for a scheduled workflow")
		}
		if err := validateCron(o.CronExpression); err != nil {
			return err
		}
	}
	if o.RequiresApproval != nil && s.Workflow == nil {
		return invalid("APPROVAL_NOT_APPLICABLE", "requires_approval can only be overridden when the SOP has a workflow")
	}
	return nil
}

// Render produces the concrete agent and workflow configs one client gets:
// placeholders filled (override, else default) and overrides applied, with
// the agents API's own defaults filled in for anything the spec left unset.
func Render(s Spec, o Overrides) (AgentConfig, *WorkflowConfig) {
	values := map[string]string{}
	for _, p := range s.Parameters {
		values[p.Key] = p.Default
	}
	for k, v := range o.Params {
		if _, ok := values[k]; ok {
			values[k] = v
		}
	}
	fill := func(t string) string {
		return placeholderPattern.ReplaceAllStringFunc(t, func(m string) string {
			return values[placeholderPattern.FindStringSubmatch(m)[1]]
		})
	}

	a := s.Agent
	a.SystemPrompt = fill(a.SystemPrompt)
	if a.AgentType == "" {
		a.AgentType = DefaultAgentType
	}
	if a.Model == "" {
		a.Model = DefaultModel
	}
	if a.MaxOutputTokens == nil {
		v := DefaultMaxOutputTokens
		a.MaxOutputTokens = &v
	}
	if a.Temperature == nil {
		v := DefaultTemperature
		a.Temperature = &v
	}
	a.PolicyScope.AllowedTools = append([]string(nil), a.PolicyScope.AllowedTools...)
	if o.MaxCostPerRunUSD != nil {
		a.PolicyScope.MaxCostPerRunUSD = o.MaxCostPerRunUSD
	}
	if o.MaxToolCalls != nil {
		a.PolicyScope.MaxToolCalls = o.MaxToolCalls
	}

	if s.Workflow == nil {
		return a, nil
	}
	w := *s.Workflow
	if w.TaskInputTemplate != nil {
		t := fill(*w.TaskInputTemplate)
		w.TaskInputTemplate = &t
	}
	if o.CronExpression != nil && w.TriggerType == "scheduled" {
		w.CronExpression = o.CronExpression
	}
	if o.RequiresApproval != nil {
		w.RequiresApproval = *o.RequiresApproval
	}
	return a, &w
}

// Placeholders returns the distinct {{keys}} used in t, sorted.
func Placeholders(t string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(t, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	sort.Strings(keys)
	return keys
}

// RequiredServices is the set of integrations ("slack", "stripe", ...) the
// agent's tools need connected in a workspace to actually run.
func RequiredServices(a AgentConfig) []string {
	seen := map[string]bool{}
	var services []string
	for _, id := range a.PolicyScope.AllowedTools {
		service, _, ok := strings.Cut(id, ".")
		if ok && !seen[service] {
			seen[service] = true
			services = append(services, service)
		}
	}
	sort.Strings(services)
	if services == nil {
		services = []string{}
	}
	return services
}

// MissingServices is RequiredServices minus what the workspace has connected.
func MissingServices(a AgentConfig, connected map[string]bool) []string {
	missing := []string{}
	for _, s := range RequiredServices(a) {
		if !connected[s] {
			missing = append(missing, s)
		}
	}
	return missing
}

func validatePolicyScope(ps PolicyScope, knownTools map[string]bool) error {
	if len(ps.AllowedTools) == 0 {
		return invalid("NO_ALLOWED_TOOLS", "At least one allowed tool is required")
	}
	if ps.MaxCostPerRunUSD != nil && *ps.MaxCostPerRunUSD <= 0 {
		return invalid("INVALID_COST_CAP", "max_cost_per_run_usd must be a positive number")
	}
	if ps.MaxToolCalls != nil && *ps.MaxToolCalls <= 0 {
		return invalid("INVALID_TOOL_CALL_CAP", "max_tool_calls must be a positive number")
	}
	for _, id := range ps.AllowedTools {
		if !knownTools[id] {
			return invalid("UNKNOWN_TOOL", "%q is not a recognized tool", id)
		}
	}
	return nil
}

func validateCron(expr *string) error {
	if expr == nil || strings.TrimSpace(*expr) == "" {
		return invalid("CRON_EXPRESSION_REQUIRED", "cron_expression is required for a scheduled workflow")
	}
	if _, err := cron.ParseStandard(*expr); err != nil {
		return invalid("INVALID_CRON_EXPRESSION", "Invalid cron_expression: %v", err)
	}
	return nil
}
