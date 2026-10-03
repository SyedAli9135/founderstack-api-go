//go:build integration

package runs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

func TestRunsHandler_Cancel_ViewerForbidden(t *testing.T) {
	appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
	router := testRouter(t, systemPool, appPool, cfg, graph.NewEngine(appPool))
	_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, "running")

	if _, err := systemPool.Exec(context.Background(), "update users set role = 'viewer' where clerk_user_id = $1", clerkUserID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(t, cfg, clerkUserID, http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer cancel: status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

// A finished run never publishes again, so the stream must answer with its
// terminal event and close, not hold the connection open forever.
func TestRunsHandler_Stream_FinishedRunClosesImmediately(t *testing.T) {
	for _, tc := range []struct {
		status    string
		wantEvent string
	}{{"completed", "event: complete"}, {"failed", "event: error"}, {"cancelled", "event: error"}} {
		t.Run(tc.status, func(t *testing.T) {
			appPool, systemPool, cfg := testAppPool(t), testSystemPool(t), testConfig(t)
			router := testRouter(t, systemPool, appPool, cfg, graph.NewEngine(appPool))
			_, clerkUserID, runID := testOrgUserAgentWorkflowRun(t, systemPool, tc.status)

			srv := httptest.NewServer(router)
			defer srv.Close()
			token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/runs/"+runID.String()+"/stream", nil)
			req.Header.Set("Authorization", "Bearer "+token)

			// The short timeout is the assertion: before the fix this request
			// never returned, because nothing would ever publish to the run.
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request did not complete: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("stream did not end: %v", err)
			}
			if !strings.Contains(string(body), tc.wantEvent) || !strings.Contains(string(body), tc.status) {
				t.Fatalf("body = %q, want %q mentioning %q", body, tc.wantEvent, tc.status)
			}
		})
	}
}
