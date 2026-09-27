package sop

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var known = map[string]bool{"slack.post_message": true, "stripe.list_payments": true}

func ptr[T any](v T) *T { return &v }

func baseSpec() Spec {
	return Spec{
		Agent: AgentConfig{
			Name:         "Weekly Close Agent",
			SystemPrompt: "You run the weekly financial close and post the summary to {{slack_channel}} every week.",
			PolicyScope:  PolicyScope{AllowedTools: []string{"slack.post_message", "stripe.list_payments"}, MaxCostPerRunUSD: ptr(2.0)},
		},
		Workflow: &WorkflowConfig{
			Name: "Weekly Close", TriggerType: "scheduled", CronExpression: ptr("0 9 * * 1"),
			TaskInputTemplate: ptr("Close the books and post to {{slack_channel}} for {{client_name}}"),
		},
		Parameters: []Parameter{{Key: "slack_channel", Default: "#finance"}, {Key: "client_name", Default: "the client"}},
	}
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return "NON_SOP_ERROR"
	}
	return ""
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"valid", func(*Spec) {}, ""},
		{"valid without a workflow", func(s *Spec) { s.Workflow = nil; s.Parameters = s.Parameters[:1] }, ""},
		{"missing agent name", func(s *Spec) { s.Agent.Name = " " }, "INVALID_AGENT_NAME"},
		{"short prompt", func(s *Spec) { s.Agent.SystemPrompt = "too short" }, "SYSTEM_PROMPT_TOO_SHORT"},
		{"no tools", func(s *Spec) { s.Agent.PolicyScope.AllowedTools = nil }, "NO_ALLOWED_TOOLS"},
		{"unknown tool", func(s *Spec) { s.Agent.PolicyScope.AllowedTools = []string{"twitter.post"} }, "UNKNOWN_TOOL"},
		{"zero cost cap", func(s *Spec) { s.Agent.PolicyScope.MaxCostPerRunUSD = ptr(0.0) }, "INVALID_COST_CAP"},
		{"negative tool cap", func(s *Spec) { s.Agent.PolicyScope.MaxToolCalls = ptr(int32(-1)) }, "INVALID_TOOL_CALL_CAP"},
		{"bad trigger", func(s *Spec) { s.Workflow.TriggerType = "hourly" }, "UNKNOWN_TRIGGER_TYPE"},
		{"scheduled without cron", func(s *Spec) { s.Workflow.CronExpression = nil }, "CRON_EXPRESSION_REQUIRED"},
		{"bad cron", func(s *Spec) { s.Workflow.CronExpression = ptr("every monday") }, "INVALID_CRON_EXPRESSION"},
		{"missing workflow name", func(s *Spec) { s.Workflow.Name = "" }, "INVALID_WORKFLOW_NAME"},
		{"negative minutes", func(s *Spec) { s.Workflow.EstimatedManualMinutes = ptr(int32(-5)) }, "INVALID_MANUAL_MINUTES"},
		{"bad param key", func(s *Spec) { s.Parameters[0].Key = "Slack-Channel" }, "INVALID_PARAMETER_KEY"},
		{"duplicate param", func(s *Spec) { s.Parameters[1].Key = "slack_channel" }, "DUPLICATE_PARAMETER_KEY"},
		{"undeclared in prompt", func(s *Spec) { s.Parameters = s.Parameters[1:] }, "UNDECLARED_PARAMETER"},
		{"undeclared in task input", func(s *Spec) { s.Parameters = s.Parameters[:1] }, "UNDECLARED_PARAMETER"},
		{"manual trigger needs no cron", func(s *Spec) { s.Workflow.TriggerType = "manual"; s.Workflow.CronExpression = nil }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSpec()
			tc.mutate(&s)
			if got := code(Validate(s, known)); got != tc.want {
				t.Fatalf("Validate() code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateOverrides(t *testing.T) {
	tests := []struct {
		name string
		spec func() Spec
		o    Overrides
		want string
	}{
		{"valid", baseSpec, Overrides{Params: map[string]string{"slack_channel": "#acme"}, MaxCostPerRunUSD: ptr(5.0), CronExpression: ptr("0 8 * * 1"), RequiresApproval: ptr(true)}, ""},
		{"unknown param", baseSpec, Overrides{Params: map[string]string{"channel": "#x"}}, "UNKNOWN_PARAMETER"},
		{"bad cost cap", baseSpec, Overrides{MaxCostPerRunUSD: ptr(-1.0)}, "INVALID_COST_CAP"},
		{"bad tool cap", baseSpec, Overrides{MaxToolCalls: ptr(int32(0))}, "INVALID_TOOL_CALL_CAP"},
		{"bad cron", baseSpec, Overrides{CronExpression: ptr("nope")}, "INVALID_CRON_EXPRESSION"},
		{"cron on manual workflow", func() Spec { s := baseSpec(); s.Workflow.TriggerType = "manual"; return s }, Overrides{CronExpression: ptr("0 8 * * 1")}, "SCHEDULE_NOT_APPLICABLE"},
		{"approval without workflow", func() Spec { s := baseSpec(); s.Workflow = nil; return s }, Overrides{RequiresApproval: ptr(true)}, "APPROVAL_NOT_APPLICABLE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := code(ValidateOverrides(tc.spec(), tc.o)); got != tc.want {
				t.Fatalf("ValidateOverrides() code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRender_DefaultsAndOverrides(t *testing.T) {
	s := baseSpec()

	a, w := Render(s, Overrides{})
	if !strings.Contains(a.SystemPrompt, "#finance") || strings.Contains(a.SystemPrompt, "{{") {
		t.Fatalf("defaults not filled: %q", a.SystemPrompt)
	}
	if a.Model != DefaultModel || a.AgentType != DefaultAgentType || *a.MaxOutputTokens != DefaultMaxOutputTokens || *a.Temperature != DefaultTemperature {
		t.Fatalf("agents-API defaults not applied: %+v", a)
	}
	if *w.TaskInputTemplate != "Close the books and post to #finance for the client" {
		t.Fatalf("task input = %q", *w.TaskInputTemplate)
	}

	a, w = Render(s, Overrides{
		Params:           map[string]string{"slack_channel": "#acme-finance", "client_name": "Acme"},
		MaxCostPerRunUSD: ptr(5.0), MaxToolCalls: ptr(int32(12)),
		CronExpression: ptr("0 7 * * 5"), RequiresApproval: ptr(true),
	})
	if !strings.Contains(a.SystemPrompt, "#acme-finance") || *w.TaskInputTemplate != "Close the books and post to #acme-finance for Acme" {
		t.Fatalf("param overrides not applied: %q / %q", a.SystemPrompt, *w.TaskInputTemplate)
	}
	if *a.PolicyScope.MaxCostPerRunUSD != 5 || *a.PolicyScope.MaxToolCalls != 12 || *w.CronExpression != "0 7 * * 5" || !w.RequiresApproval {
		t.Fatalf("structured overrides not applied: %+v %+v", a.PolicyScope, w)
	}
}

func TestRender_DoesNotMutateSpec(t *testing.T) {
	s := baseSpec()
	Render(s, Overrides{Params: map[string]string{"slack_channel": "#x"}, MaxCostPerRunUSD: ptr(9.0), CronExpression: ptr("0 1 * * *")})
	if !reflect.DeepEqual(s, baseSpec()) {
		t.Fatal("Render mutated the spec it was given")
	}
}

func TestRender_StaleOverridesSurviveVersionChange(t *testing.T) {
	// A newer version dropped {{client_name}}; the stored override for it is
	// ignored, the still-declared one still applies.
	s := baseSpec()
	s.Parameters = s.Parameters[:1]
	s.Workflow.TaskInputTemplate = ptr("Post to {{slack_channel}}")
	o := Overrides{Params: map[string]string{"slack_channel": "#acme", "client_name": "Acme"}}
	_, w := Render(s, o)
	if *w.TaskInputTemplate != "Post to #acme" {
		t.Fatalf("task input = %q", *w.TaskInputTemplate)
	}
}

func TestPlaceholdersAndServices(t *testing.T) {
	if got := Placeholders("{{b}} and {{ a }} and {{b}} but not {{Bad}}"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Placeholders = %v", got)
	}
	a := baseSpec().Agent
	if got := RequiredServices(a); !reflect.DeepEqual(got, []string{"slack", "stripe"}) {
		t.Fatalf("RequiredServices = %v", got)
	}
	if got := MissingServices(a, map[string]bool{"slack": true}); !reflect.DeepEqual(got, []string{"stripe"}) {
		t.Fatalf("MissingServices = %v", got)
	}
}
