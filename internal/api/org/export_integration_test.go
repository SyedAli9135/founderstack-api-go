//go:build integration

package org

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrgHandler_Export(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	cfg := testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})
	mine := newTestOrg(t, systemPool)
	other := newTestOrg(t, systemPool)
	ctx := context.Background()

	// A secret and a foreign tenant's row must never appear in the export.
	if _, err := systemPool.Exec(ctx, `INSERT INTO api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
		VALUES ($1,'anthropic','sk-ant-xx','CIPHERTEXT-MUST-NOT-LEAK','local-aes-gcm',true)`, mine.orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := systemPool.Exec(ctx, `INSERT INTO agents (org_id, name, slug, system_prompt, model)
		VALUES ($1,'Mine Agent','mine','prompt prompt prompt prompt prompt prompt prompt prompt','mock:happy'),
		       ($2,'FOREIGN-AGENT','foreign','prompt prompt prompt prompt prompt prompt prompt prompt','mock:happy')`, mine.orgID, other.orgID); err != nil {
		t.Fatal(err)
	}

	get := func(clerkID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authedRequest(t, cfg, clerkID, http.MethodGet, "/api/v1/org/export", nil))
		return rec
	}

	t.Run("admin gets their workspace's data, scoped and without secrets", func(t *testing.T) {
		rec := get(mine.adminClerkID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Error("export should download as an attachment")
		}
		body := rec.Body.String()
		for _, forbidden := range []string{"CIPHERTEXT-MUST-NOT-LEAK", "FOREIGN-AGENT"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("export leaked %q", forbidden)
			}
		}
		var out struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, section := range []string{"organization", "members", "agents", "api_keys", "audit_logs"} {
			if _, ok := out.Data[section]; !ok {
				t.Errorf("section %q missing", section)
			}
		}
		if !strings.Contains(string(out.Data["agents"]), "Mine Agent") || !strings.Contains(string(out.Data["api_keys"]), "sk-ant-xx") {
			t.Errorf("own data missing: agents=%s keys=%s", out.Data["agents"], out.Data["api_keys"])
		}
	})

	t.Run("members and viewers are refused", func(t *testing.T) {
		for _, id := range []string{mine.memberClerkID, mine.viewerClerkID} {
			if rec := get(id); rec.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", id, rec.Code)
			}
		}
	})
}
