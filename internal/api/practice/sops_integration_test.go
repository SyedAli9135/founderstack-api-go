//go:build integration

package practice

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

const closePrompt = "You run the weekly financial close for the client and post the summary to {{slack_channel}}."

func weeklyCloseSop(name string) map[string]any {
	return map[string]any{
		"name": name, "category": "finance", "description": "Weekly close playbook",
		"agent_config": map[string]any{
			"name": "Weekly Close Agent", "system_prompt": closePrompt, "model": "mock:happy",
			"policy_scope": map[string]any{"allowed_tools": []string{"slack.post_message", "stripe.list_payments"}, "max_cost_per_run_usd": 2},
		},
		"workflow_config": map[string]any{
			"name": "Weekly Close", "trigger_type": "scheduled", "cron_expression": "0 9 * * 1",
			"task_input_template": "Close the week and post to {{slack_channel}}", "estimated_manual_minutes": 90,
		},
		"parameters": []map[string]any{{"key": "slack_channel", "label": "Slack channel", "default": "#finance"}},
	}
}

type sopResp struct {
	ID             string `json:"id"`
	CurrentVersion int32  `json:"current_version"`
	Versions       []struct {
		Version int32 `json:"version"`
	} `json:"versions"`
}

type deployResp struct {
	ID                  string   `json:"id"`
	WorkspaceID         string   `json:"workspace_id"`
	DeployedVersion     int32    `json:"deployed_version"`
	Status              string   `json:"status"`
	AgentID             string   `json:"agent_id"`
	WorkflowID          *string  `json:"workflow_id"`
	MissingIntegrations []string `json:"missing_integrations"`
}

func decode[T any](t *testing.T, env envelope) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("decode %s: %v", env.Data, err)
	}
	return v
}

// sopFixture: a practice (operator admin, assistant member) with two client
// workspaces A and B created through the real handler.
type sopFixture struct {
	*fixture
	r    *gin.Engine
	a, b createdWorkspace
}

func newSopFixture(t *testing.T) (*sopFixture, *pgxpool.Pool) {
	pool := testSystemPool(t)
	appPool := testAppPool(t)
	fx := newFixture(t, pool)
	r := testRouterWithApp(pool, appPool, &fakeProvisioner{})
	sf := &sopFixture{fixture: fx, r: r}
	sf.a = fx.create(t, r, "SopA")
	sf.b = fx.create(t, r, "SopB")
	// Deployed agents/workflows/deployments don't cascade from organizations
	// the way the org cleanup assumes; clear them first (runs last).
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range []string{sf.a.ID, sf.b.ID, fx.practiceID.String()} {
			_, _ = pool.Exec(ctx, "delete from sop_deployments where target_org_id = $1", id)
			_, _ = pool.Exec(ctx, "delete from workflows where org_id = $1", id)
			_, _ = pool.Exec(ctx, "delete from agents where org_id = $1", id)
		}
		_, _ = pool.Exec(ctx, "delete from sop_playbooks where practice_id = $1", fx.practiceID)
	})
	return sf, pool
}

func (sf *sopFixture) do(t *testing.T, method, path string, body any) (int, envelope) {
	t.Helper()
	return call(t, sf.r, sf.operator, sf.practiceClerkID, method, path, body)
}

func agentPrompt(t *testing.T, pool *pgxpool.Pool, agentID string) (prompt string, active bool, policy string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		"select system_prompt, is_active, policy_scope::text from agents where id = $1", agentID,
	).Scan(&prompt, &active, &policy); err != nil {
		t.Fatalf("load agent: %v", err)
	}
	return
}

func TestSops_EndToEnd(t *testing.T) {
	sf, pool := newSopFixture(t)
	ctx := context.Background()

	code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops", weeklyCloseSop("Weekly Financial Close"))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, env.Error.Code)
	}
	s := decode[sopResp](t, env)
	if s.CurrentVersion != 1 || len(s.Versions) != 1 {
		t.Fatalf("new SOP = %+v, want version 1 with one history entry", s)
	}
	base := "/api/v1/practice/sops/" + s.ID

	var depA, depB deployResp
	t.Run("deploy to A with overrides renders params and structured overrides", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPost, base+"/deploy", map[string]any{
			"target_org_id": sf.a.ID,
			"parameter_overrides": map[string]any{
				"params": map[string]string{"slack_channel": "#acme-finance"}, "max_cost_per_run_usd": 5, "cron_expression": "0 7 * * 5",
			},
		})
		if code != http.StatusCreated {
			t.Fatalf("deploy A: %d %s", code, env.Error.Code)
		}
		depA = decode[deployResp](t, env)
		prompt, active, policy := agentPrompt(t, pool, depA.AgentID)
		if !active || !strings.Contains(prompt, "#acme-finance") || strings.Contains(prompt, "{{") || !strings.Contains(policy, `"max_cost_per_run_usd": 5`) {
			t.Fatalf("A's agent = (%v, %q, %s)", active, prompt, policy)
		}
		var cronExpr, task, org string
		_ = pool.QueryRow(ctx, "select cron_expression, task_input_template, org_id::text from workflows where id = $1", *depA.WorkflowID).Scan(&cronExpr, &task, &org)
		if cronExpr != "0 7 * * 5" || task != "Close the week and post to #acme-finance" || org != sf.a.ID {
			t.Fatalf("A's workflow = (%q, %q, org %s)", cronExpr, task, org)
		}
		if len(depA.MissingIntegrations) != 2 {
			t.Fatalf("missing_integrations = %v, want slack+stripe (none connected)", depA.MissingIntegrations)
		}
	})

	t.Run("deploy to B with defaults", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPost, base+"/deploy", map[string]any{"target_org_id": sf.b.ID})
		if code != http.StatusCreated {
			t.Fatalf("deploy B: %d %s", code, env.Error.Code)
		}
		depB = decode[deployResp](t, env)
		if prompt, _, _ := agentPrompt(t, pool, depB.AgentID); !strings.Contains(prompt, "#finance") {
			t.Fatalf("B's prompt = %q, want the default channel", prompt)
		}
	})

	t.Run("deploying twice to the same workspace is refused", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPost, base+"/deploy", map[string]any{"target_org_id": sf.a.ID})
		if code != http.StatusConflict || env.Error.Code != "ALREADY_DEPLOYED" {
			t.Fatalf("got (%d, %s), want 409 ALREADY_DEPLOYED", code, env.Error.Code)
		}
	})

	t.Run("editing creates v2; only the synced client updates, keeping its overrides", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPatch, base, map[string]any{
			"agent_config": map[string]any{
				"name": "Weekly Close Agent", "model": "mock:happy",
				"system_prompt": closePrompt + " Flag any payment over $10k for review.",
				"policy_scope":  map[string]any{"allowed_tools": []string{"slack.post_message", "stripe.list_payments"}, "max_cost_per_run_usd": 2},
			},
			"changelog": "Flag large payments",
		})
		if code != http.StatusOK || decode[sopResp](t, env).CurrentVersion != 2 {
			t.Fatalf("edit: %d %s", code, env.Error.Code)
		}

		statuses := func() map[string]string {
			_, env := sf.do(t, http.MethodGet, base+"/deployments", nil)
			var out struct {
				Deployments []deployResp `json:"deployments"`
			}
			_ = json.Unmarshal(env.Data, &out)
			m := map[string]string{}
			for _, d := range out.Deployments {
				m[d.WorkspaceID] = d.Status
			}
			return m
		}
		if st := statuses(); st[sf.a.ID] != "update_available" || st[sf.b.ID] != "update_available" {
			t.Fatalf("after edit statuses = %v, want both update_available", st)
		}

		if code, env := sf.do(t, http.MethodPost, base+"/deployments/"+depA.ID+"/sync", nil); code != http.StatusOK {
			t.Fatalf("sync A: %d %s", code, env.Error.Code)
		}
		if st := statuses(); st[sf.a.ID] != "up_to_date" || st[sf.b.ID] != "update_available" {
			t.Fatalf("after syncing A statuses = %v, want A up_to_date, B still update_available", st)
		}
		promptA, _, policyA := agentPrompt(t, pool, depA.AgentID)
		if !strings.Contains(promptA, "Flag any payment") || !strings.Contains(promptA, "#acme-finance") || !strings.Contains(policyA, `"max_cost_per_run_usd": 5`) {
			t.Fatalf("A after sync = (%q, %s), want v2 text with A's overrides preserved", promptA, policyA)
		}
		if promptB, _, _ := agentPrompt(t, pool, depB.AgentID); strings.Contains(promptB, "Flag any payment") {
			t.Fatalf("B changed without a sync: %q", promptB)
		}
	})

	t.Run("changing B's overrides applies them at B's current version, without upgrading it", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPatch, base+"/deployments/"+depB.ID, map[string]any{
			"parameter_overrides": map[string]any{"params": map[string]string{"slack_channel": "#b-ops"}},
		})
		if code != http.StatusOK {
			t.Fatalf("patch overrides: %d %s", code, env.Error.Code)
		}
		promptB, _, _ := agentPrompt(t, pool, depB.AgentID)
		if !strings.Contains(promptB, "#b-ops") || strings.Contains(promptB, "Flag any payment") {
			t.Fatalf("B = %q, want new channel on v1 text", promptB)
		}
	})

	t.Run("sync never un-pauses a workflow the client paused", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "update workflows set is_active = false where id = $1", *depB.WorkflowID); err != nil {
			t.Fatal(err)
		}
		if code, env := sf.do(t, http.MethodPost, base+"/deployments/"+depB.ID+"/sync", nil); code != http.StatusOK {
			t.Fatalf("sync B: %d %s", code, env.Error.Code)
		}
		var active bool
		_ = pool.QueryRow(ctx, "select is_active from workflows where id = $1", *depB.WorkflowID).Scan(&active)
		if active {
			t.Fatal("sync re-activated a paused workflow")
		}
	})

	t.Run("client-side labels and portfolio counts", func(t *testing.T) {
		var aID pgtype.UUID
		_ = aID.Scan(sf.a.ID)
		var managed []dbgen.ListSopManagedResourcesRow
		appPool := testAppPool(t)
		if err := tenant.WithTx(ctx, appPool, aID, func(ctx context.Context, q *dbgen.Queries) error {
			var err error
			managed, err = q.ListSopManagedResources(ctx, aID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(managed) != 1 || managed[0].SopName != "Weekly Financial Close" || managed[0].DeployedVersion != 2 {
			t.Fatalf("A's managed resources = %+v", managed)
		}
		_, out := list(t, sf.r, sf.operator, sf.practiceClerkID)
		for _, w := range out.Workspaces {
			if w.Stats.SopsDeployed != 1 {
				t.Fatalf("%s sops_deployed = %d, want 1", w.Name, w.Stats.SopsDeployed)
			}
		}
	})

	t.Run("a removed agent makes the deployment broken; sync refuses", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops", weeklyCloseSop("Second Close"))
		if code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, env.Error.Code)
		}
		second := decode[sopResp](t, env)
		// Different agent name so it doesn't collide with the first SOP's agent in A.
		body := weeklyCloseSop("Second Close")
		body["agent_config"].(map[string]any)["name"] = "Second Close Agent"
		body["changelog"] = "rename agent"
		if code, env := sf.do(t, http.MethodPatch, "/api/v1/practice/sops/"+second.ID, map[string]any{"agent_config": body["agent_config"]}); code != http.StatusOK {
			t.Fatalf("rename: %d %s", code, env.Error.Code)
		}
		code, env = sf.do(t, http.MethodPost, "/api/v1/practice/sops/"+second.ID+"/deploy", map[string]any{"target_org_id": sf.a.ID})
		if code != http.StatusCreated {
			t.Fatalf("deploy second: %d %s", code, env.Error.Code)
		}
		dep := decode[deployResp](t, env)
		if _, err := pool.Exec(ctx, "update agents set is_active = false where id = $1", dep.AgentID); err != nil {
			t.Fatal(err)
		}
		_, env = sf.do(t, http.MethodGet, "/api/v1/practice/sops/"+second.ID+"/deployments", nil)
		if !strings.Contains(string(env.Data), `"status":"broken"`) {
			t.Fatalf("deployments = %s, want status broken", env.Data)
		}
		code, env = sf.do(t, http.MethodPost, "/api/v1/practice/sops/"+second.ID+"/deployments/"+dep.ID+"/sync", nil)
		if code != http.StatusConflict || env.Error.Code != "DEPLOYMENT_BROKEN" {
			t.Fatalf("sync broken: (%d, %s), want 409 DEPLOYMENT_BROKEN", code, env.Error.Code)
		}
	})

	t.Run("un-deploy from B deactivates only B's copy", func(t *testing.T) {
		if code, env := sf.do(t, http.MethodDelete, base+"/deployments/"+depB.ID, nil); code != http.StatusOK {
			t.Fatalf("undeploy: %d %s", code, env.Error.Code)
		}
		if _, active, _ := agentPrompt(t, pool, depB.AgentID); active {
			t.Fatal("B's agent still active after un-deploy")
		}
		if _, active, _ := agentPrompt(t, pool, depA.AgentID); !active {
			t.Fatal("A's agent was affected by un-deploying B")
		}
	})

	t.Run("deleting the SOP from the library leaves deployed agents running", func(t *testing.T) {
		if code, env := sf.do(t, http.MethodDelete, base, nil); code != http.StatusOK {
			t.Fatalf("delete: %d %s", code, env.Error.Code)
		}
		if _, active, _ := agentPrompt(t, pool, depA.AgentID); !active {
			t.Fatal("A's agent was deactivated by deleting the SOP")
		}
		var labels int
		_ = pool.QueryRow(ctx, "select count(*) from sop_deployments where agent_id = $1 and is_active", depA.AgentID).Scan(&labels)
		if labels != 0 {
			t.Fatal("A's agent still labeled as SOP-managed after the SOP was deleted")
		}
		if code, _ := sf.do(t, http.MethodGet, base, nil); code != http.StatusNotFound {
			t.Fatalf("deleted SOP still readable: %d", code)
		}
	})
}

func TestSops_ValidationAndAccess(t *testing.T) {
	sf, pool := newSopFixture(t)
	create := func(body map[string]any) (int, string) {
		code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops", body)
		return code, env.Error.Code
	}

	bad := weeklyCloseSop("Bad")
	bad["parameters"] = []map[string]any{}
	if code, c := create(bad); code != http.StatusBadRequest || c != "UNDECLARED_PARAMETER" {
		t.Fatalf("undeclared param: (%d, %s)", code, c)
	}
	bad = weeklyCloseSop("Bad")
	bad["agent_config"].(map[string]any)["policy_scope"] = map[string]any{"allowed_tools": []string{"twitter.post"}}
	if code, c := create(bad); code != http.StatusBadRequest || c != "UNKNOWN_TOOL" {
		t.Fatalf("unknown tool: (%d, %s)", code, c)
	}
	if code, c := create(weeklyCloseSop("Dup")); code != http.StatusCreated {
		t.Fatalf("create: (%d, %s)", code, c)
	}
	if code, c := create(weeklyCloseSop("Dup")); code != http.StatusConflict || c != "DUPLICATE_SOP_NAME" {
		t.Fatalf("duplicate: (%d, %s)", code, c)
	}

	_, env := sf.do(t, http.MethodGet, "/api/v1/practice/sops", nil)
	var lib struct {
		Sops []sopResp `json:"sops"`
	}
	_ = json.Unmarshal(env.Data, &lib)
	sopID := lib.Sops[0].ID

	t.Run("a practice member can read the library but not change it", func(t *testing.T) {
		if code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops/"+sopID+"/deploy", map[string]any{"target_org_id": sf.a.ID}); code != http.StatusCreated {
			t.Fatalf("operator deploy: %d %s", code, env.Error.Code)
		}
		code, env := call(t, sf.r, sf.assistant, sf.practiceClerkID, http.MethodGet, "/api/v1/practice/sops", nil)
		if code != http.StatusOK {
			t.Fatalf("member list: %d", code)
		}
		// The member isn't in workspace A, so A's deployment isn't counted for
		// them — matching the (empty) deployments list they'd see.
		if strings.Contains(string(env.Data), `"active_deployments":1`) {
			t.Fatalf("member's library counts a deployment in a workspace they can't see: %s", env.Data)
		}
		code, env = call(t, sf.r, sf.assistant, sf.practiceClerkID, http.MethodPost, "/api/v1/practice/sops", weeklyCloseSop("Nope"))
		if code != http.StatusForbidden || env.Error.Code != "NOT_AUTHORIZED" {
			t.Fatalf("member create: (%d, %s)", code, env.Error.Code)
		}
	})

	t.Run("deploy targets must be this practice's client workspaces", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops/"+sopID+"/deploy", map[string]any{"target_org_id": sf.practiceID.String()})
		if code != http.StatusBadRequest || env.Error.Code != "TARGET_NOT_CLIENT_WORKSPACE" {
			t.Fatalf("deploy to practice: (%d, %s)", code, env.Error.Code)
		}
		code, env = sf.do(t, http.MethodPost, "/api/v1/practice/sops/"+sopID+"/deploy", map[string]any{
			"target_org_id": sf.a.ID, "parameter_overrides": map[string]any{"params": map[string]string{"nope": "x"}},
		})
		if code != http.StatusBadRequest || env.Error.Code != "UNKNOWN_PARAMETER" {
			t.Fatalf("bad override: (%d, %s)", code, env.Error.Code)
		}
	})

	t.Run("another practice can't see or use this practice's SOPs", func(t *testing.T) {
		code, _ := call(t, sf.r, sf.outsid, sf.otherPracticeClerkID, http.MethodGet, "/api/v1/practice/sops/"+sopID, nil)
		if code != http.StatusNotFound {
			t.Fatalf("outsider get: %d, want 404", code)
		}
	})

	t.Run("a client workspace's own RLS context can't read the practice's playbooks", func(t *testing.T) {
		appPool := testAppPool(t)
		var aID pgtype.UUID
		_ = aID.Scan(sf.a.ID)
		var n int
		if err := appPoolCount(context.Background(), appPool, aID, &n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("client workspace sees %d playbooks, want 0", n)
		}
	})

	t.Run("promote an existing agent + workflow into a SOP", func(t *testing.T) {
		ctx := context.Background()
		var agentID, workflowID string
		if err := pool.QueryRow(ctx,
			`insert into agents (org_id, name, slug, system_prompt, model, policy_scope)
			 values ($1, 'Invoice Chaser', 'invoice-chaser', 'You chase overdue invoices politely and log every reminder you send.', 'claude-sonnet-5', '{"allowed_tools":["stripe.list_payments"]}')
			 returning id`, sf.practiceID).Scan(&agentID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx,
			`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition, task_input_template)
			 values ($1, $2, 'Chase invoices', 'manual', '{}', 'Chase this week''s overdue invoices') returning id`,
			sf.practiceID, agentID).Scan(&workflowID); err != nil {
			t.Fatal(err)
		}
		code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops", map[string]any{
			"name": "Invoice Chasing", "source": map[string]any{"agent_id": agentID, "workflow_id": workflowID},
		})
		if code != http.StatusCreated {
			t.Fatalf("promote: %d %s", code, env.Error.Code)
		}
		var d struct {
			AgentConfig struct {
				Name         string `json:"name"`
				SystemPrompt string `json:"system_prompt"`
			} `json:"agent_config"`
			WorkflowConfig struct {
				Name              string `json:"name"`
				TaskInputTemplate string `json:"task_input_template"`
			} `json:"workflow_config"`
		}
		_ = json.Unmarshal(env.Data, &d)
		if d.AgentConfig.Name != "Invoice Chaser" || d.WorkflowConfig.Name != "Chase invoices" || !strings.Contains(d.WorkflowConfig.TaskInputTemplate, "overdue") {
			t.Fatalf("promoted SOP = %+v", d)
		}
	})
}

func appPoolCount(ctx context.Context, appPool *pgxpool.Pool, orgID pgtype.UUID, n *int) error {
	tx, err := appPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select set_config('app.current_org_id', $1, true)", orgID.String()); err != nil {
		return err
	}
	return tx.QueryRow(ctx, "select count(*) from sop_playbooks").Scan(n)
}

func TestSops_WorkflowAddedAndRemovedAcrossVersions(t *testing.T) {
	sf, pool := newSopFixture(t)
	ctx := context.Background()

	agentOnly := weeklyCloseSop("Agent Only Close")
	delete(agentOnly, "workflow_config")
	code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops", agentOnly)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, env.Error.Code)
	}
	base := "/api/v1/practice/sops/" + decode[sopResp](t, env).ID

	code, env = sf.do(t, http.MethodPost, base+"/deploy", map[string]any{
		"target_org_id": sf.a.ID, "parameter_overrides": map[string]any{"params": map[string]string{"slack_channel": "#a"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("deploy: %d %s", code, env.Error.Code)
	}
	dep := decode[deployResp](t, env)
	if dep.WorkflowID != nil {
		t.Fatalf("agent-only SOP deployed a workflow: %v", *dep.WorkflowID)
	}
	deploymentWorkflow := func() pgtype.UUID {
		var id pgtype.UUID
		_ = pool.QueryRow(ctx, "select workflow_id from sop_deployments where id = $1", dep.ID).Scan(&id)
		return id
	}

	var workflowID string
	t.Run("a version that adds a workflow creates it in the client on sync", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPatch, base, map[string]any{
			"workflow_config": weeklyCloseSop("x")["workflow_config"], "changelog": "add schedule",
		})
		if code != http.StatusOK || decode[sopResp](t, env).CurrentVersion != 2 {
			t.Fatalf("add workflow: %d %s", code, env.Error.Code)
		}
		if code, env := sf.do(t, http.MethodPost, base+"/deployments/"+dep.ID+"/sync", nil); code != http.StatusOK {
			t.Fatalf("sync: %d %s", code, env.Error.Code)
		}
		wf := deploymentWorkflow()
		if !wf.Valid {
			t.Fatal("deployment has no workflow after syncing a version that added one")
		}
		workflowID = wf.String()
		var org, task, agentID string
		var active bool
		_ = pool.QueryRow(ctx, "select org_id::text, task_input_template, agent_id::text, is_active from workflows where id = $1", workflowID).Scan(&org, &task, &agentID, &active)
		if org != sf.a.ID || agentID != dep.AgentID || !active || task != "Close the week and post to #a" {
			t.Fatalf("created workflow = (org %s, agent %s, active %v, %q)", org, agentID, active, task)
		}
	})

	t.Run("a version that drops the workflow pauses the client's copy on sync", func(t *testing.T) {
		code, env := sf.do(t, http.MethodPatch, base, map[string]any{"remove_workflow": true, "changelog": "agent only again"})
		if code != http.StatusOK || decode[sopResp](t, env).CurrentVersion != 3 {
			t.Fatalf("remove workflow: %d %s", code, env.Error.Code)
		}
		if code, env := sf.do(t, http.MethodPost, base+"/deployments/"+dep.ID+"/sync", nil); code != http.StatusOK {
			t.Fatalf("sync: %d %s", code, env.Error.Code)
		}
		if deploymentWorkflow().Valid {
			t.Fatal("deployment still linked to a workflow the SOP no longer has")
		}
		var active bool
		_ = pool.QueryRow(ctx, "select is_active from workflows where id = $1", workflowID).Scan(&active)
		if active {
			t.Fatal("the dropped workflow is still active in the client")
		}
	})

	t.Run("a version dropping an overridden parameter still syncs", func(t *testing.T) {
		body := weeklyCloseSop("x")
		agent := body["agent_config"].(map[string]any)
		agent["system_prompt"] = "You run the weekly financial close for the client and email the summary to the owner."
		code, env := sf.do(t, http.MethodPatch, base, map[string]any{
			"agent_config": agent, "parameters": []map[string]any{}, "changelog": "no slack",
		})
		if code != http.StatusOK {
			t.Fatalf("drop param: %d %s", code, env.Error.Code)
		}
		if code, env := sf.do(t, http.MethodPost, base+"/deployments/"+dep.ID+"/sync", nil); code != http.StatusOK {
			t.Fatalf("sync with a stale override: %d %s", code, env.Error.Code)
		}
		if prompt, _, _ := agentPrompt(t, pool, dep.AgentID); !strings.Contains(prompt, "email the summary") {
			t.Fatalf("prompt = %q, want v4 text", prompt)
		}
	})
}

func TestSops_DeployFailuresLeaveNothingBehind(t *testing.T) {
	sf, pool := newSopFixture(t)
	ctx := context.Background()
	code, env := sf.do(t, http.MethodPost, "/api/v1/practice/sops", weeklyCloseSop("Clashing Close"))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, env.Error.Code)
	}
	base := "/api/v1/practice/sops/" + decode[sopResp](t, env).ID
	countIn := func(org, table string) int {
		var n int
		_ = pool.QueryRow(ctx, "select count(*) from "+table+" where org_id = $1", org).Scan(&n)
		return n
	}

	t.Run("an agent-name clash is refused and rolls back entirely", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `insert into agents (org_id, name, slug, system_prompt) values ($1, 'Weekly Close Agent', 'weekly-close-agent', 'An agent the client built by hand with the same name.')`, sf.a.ID); err != nil {
			t.Fatal(err)
		}
		agentsBefore, workflowsBefore := countIn(sf.a.ID, "agents"), countIn(sf.a.ID, "workflows")
		code, env := sf.do(t, http.MethodPost, base+"/deploy", map[string]any{"target_org_id": sf.a.ID})
		if code != http.StatusConflict || env.Error.Code != "DUPLICATE_AGENT_NAME" {
			t.Fatalf("got (%d, %s), want 409 DUPLICATE_AGENT_NAME", code, env.Error.Code)
		}
		if countIn(sf.a.ID, "agents") != agentsBefore || countIn(sf.a.ID, "workflows") != workflowsBefore {
			t.Fatal("a failed deploy left rows behind")
		}
		var deps int
		_ = pool.QueryRow(ctx, "select count(*) from sop_deployments where target_org_id = $1", sf.a.ID).Scan(&deps)
		if deps != 0 {
			t.Fatal("a failed deploy left a deployment row")
		}
	})

	t.Run("the client's agent limit is enforced", func(t *testing.T) {
		if _, err := pool.Exec(ctx, "update organizations set max_agents = 0 where id = $1", sf.b.ID); err != nil {
			t.Fatal(err)
		}
		code, env := sf.do(t, http.MethodPost, base+"/deploy", map[string]any{"target_org_id": sf.b.ID})
		if code != http.StatusBadRequest || env.Error.Code != "PLAN_LIMIT_REACHED" {
			t.Fatalf("got (%d, %s), want 400 PLAN_LIMIT_REACHED", code, env.Error.Code)
		}
		if countIn(sf.b.ID, "agents") != 0 {
			t.Fatal("agent created despite the limit")
		}
	})
}
