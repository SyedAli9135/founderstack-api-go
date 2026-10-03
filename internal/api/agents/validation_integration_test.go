//go:build integration

package agents

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const validPrompt = "You are a careful assistant used only by the agent validation integration tests."

func validAgentBody(name string) map[string]any {
	return map[string]any{
		"name": name, "system_prompt": validPrompt,
		"policy_scope": map[string]any{"allowed_tools": []string{"stripe.get_mrr"}},
	}
}

func TestAgentsHandler_RejectsOutOfRangeFields(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, fakeToolRegistry(t))

	post := func(body map[string]any) (int, string) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", body))
		return rec.Code, rec.Body.String()
	}
	with := func(key string, value any) map[string]any {
		b := validAgentBody("Bounds Agent " + randSuffix(t))
		b[key] = value
		return b
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"name over 255", with("name", strings.Repeat("n", 256)), "INVALID_AGENT_NAME"},
		{"blank name", with("name", "   "), "INVALID_AGENT_NAME"},
		{"description over 2000", with("description", strings.Repeat("d", 2001)), "DESCRIPTION_TOO_LONG"},
		{"agent_type over 50", with("agent_type", strings.Repeat("t", 51)), "INVALID_AGENT_TYPE"},
		{"model over 100", with("model", strings.Repeat("m", 101)), "INVALID_MODEL"},
		{"prompt over 20000", with("system_prompt", strings.Repeat("p", maxSystemPromptLen+1)), "SYSTEM_PROMPT_TOO_LONG"},
		{"zero max_output_tokens", with("max_output_tokens", 0), "INVALID_MAX_OUTPUT_TOKENS"},
		{"huge max_output_tokens", with("max_output_tokens", 10000000), "INVALID_MAX_OUTPUT_TOKENS"},
		{"negative temperature", with("temperature", -0.5), "INVALID_TEMPERATURE"},
		{"temperature over 2", with("temperature", 9), "INVALID_TEMPERATURE"},
	} {
		status, body := post(tc.body)
		if status != http.StatusBadRequest || !strings.Contains(body, tc.code) {
			t.Errorf("%s: got (%d, %s), want 400 %s", tc.name, status, body, tc.code)
		}
	}

	t.Run("a 255-character name is accepted and its slug fits the column", func(t *testing.T) {
		status, body := post(validAgentBody(strings.Repeat("a", 255)))
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (was a 500 before the slug was capped); body = %s", status, body)
		}
	})
}

func TestAgentsHandler_UpdateRejectsOutOfRangeFields(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	_, clerkUserID := testOrgAndUser(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, fakeToolRegistry(t))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/agents", validAgentBody("Update Bounds "+randSuffix(t))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := jsonField(t, rec.Body.Bytes(), "id")

	for name, body := range map[string]map[string]any{
		"blank name":        {"name": ""},
		"name over 255":     {"name": strings.Repeat("n", 256)},
		"temperature 5":     {"temperature": 5},
		"prompt over limit": {"system_prompt": strings.Repeat("p", maxSystemPromptLen+1)},
		"tokens zero":       {"max_output_tokens": 0},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPatch, "/api/v1/agents/"+id, body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %s", name, rec.Code, rec.Body.String())
		}
	}
}

// jsonField returns data.<field> from a standard response envelope.
func jsonField(t *testing.T, body []byte, field string) string {
	t.Helper()
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	v, _ := data[field].(string)
	return v
}
