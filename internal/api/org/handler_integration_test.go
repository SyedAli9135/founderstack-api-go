//go:build integration

package org

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// fakeMembershipSyncer never calls Clerk's real API — tests must not risk
// mutating a real org's real membership data (see MembershipSyncer's own
// doc comment). Records calls so a test can assert the sync was attempted
// with the right arguments, independent of whether it "succeeds."
type fakeMembershipSyncer struct {
	mu          sync.Mutex
	updateCalls []struct{ clerkOrgID, clerkUserID, role string }
	removeCalls []struct{ clerkOrgID, clerkUserID string }
	updateErr   error
	removeErr   error
}

func (f *fakeMembershipSyncer) UpdateRole(ctx context.Context, clerkOrgID, clerkUserID, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, struct{ clerkOrgID, clerkUserID, role string }{clerkOrgID, clerkUserID, role})
	return f.updateErr
}

func (f *fakeMembershipSyncer) Remove(ctx context.Context, clerkOrgID, clerkUserID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, struct{ clerkOrgID, clerkUserID string }{clerkOrgID, clerkUserID})
	return f.removeErr
}

func testAppPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to app test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testSystemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to system test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}
}

// fakeInvitationLister never calls Clerk's real API — same reasoning as
// fakeMembershipSyncer. Tests that don't care about invitations at all use
// testRouter's default empty one; TestOrgHandler_ListInvitations* below
// construct their own to control what List returns.
type fakeInvitationLister struct {
	mu          sync.Mutex
	invitations []Invitation
	listErr     error
	revokeCalls []struct{ clerkOrgID, invitationID, requestingClerkUserID string }
	revokeErr   error
}

func (f *fakeInvitationLister) List(ctx context.Context, clerkOrgID string) ([]Invitation, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.invitations, nil
}

func (f *fakeInvitationLister) Revoke(ctx context.Context, clerkOrgID, invitationID, requestingClerkUserID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls = append(f.revokeCalls, struct{ clerkOrgID, invitationID, requestingClerkUserID string }{clerkOrgID, invitationID, requestingClerkUserID})
	return f.revokeErr
}

func testRouter(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, syncer MembershipSyncer) *gin.Engine {
	t.Helper()
	return testRouterFull(t, systemPool, appPool, cfg, syncer, &fakeInvitationLister{})
}

func testRouterFull(t *testing.T, systemPool, appPool *pgxpool.Pool, cfg *config.Config, syncer MembershipSyncer, invitations InvitationLister) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())

	authed := r.Group("/api/v1")
	authed.Use(middleware.RequireAuth(systemPool, cfg))
	NewHandler(appPool, syncer, invitations).Register(authed)

	return r
}

func authedRequest(t *testing.T, cfg *config.Config, clerkUserID, method, path string, body []byte) *http.Request {
	t.Helper()
	token, err := devtoken.Sign(cfg.DevTokenSecret.Expose(), clerkUserID)
	if err != nil {
		t.Fatalf("sign dev token: %v", err)
	}
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// testOrg creates an org plus 3 real users (owner-equivalent "admin",
// "member", "viewer") — the realistic shape a real team page renders,
// exercising every role tier at once instead of one org per test.
type testOrgFixture struct {
	orgID                       pgtype.UUID
	clerkOrgID                  string
	adminClerkID, memberClerkID string
	adminID, memberID, viewerID pgtype.UUID
	viewerClerkID               string
}

func newTestOrg(t *testing.T, systemPool *pgxpool.Pool) testOrgFixture {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()
	fx := testOrgFixture{
		clerkOrgID:    "org_org_test_" + suffix,
		adminClerkID:  "user_org_test_admin_" + suffix,
		memberClerkID: "user_org_test_member_" + suffix,
		viewerClerkID: "user_org_test_viewer_" + suffix,
	}

	if err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Org Test Org', $2) returning id",
		fx.clerkOrgID, "org-test-"+suffix,
	).Scan(&fx.orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", fx.orgID)
	})

	insertUser := func(clerkID, email, role string, canManage bool) pgtype.UUID {
		var id pgtype.UUID
		if err := systemPool.QueryRow(ctx,
			`insert into users (org_id, clerk_user_id, email, role, can_manage_api_keys, can_manage_integrations, can_approve_workflows)
			 values ($1, $2, $3, $4, $5, $5, $5) returning id`,
			fx.orgID, clerkID, email, role, canManage,
		).Scan(&id); err != nil {
			t.Fatalf("insert test user %s: %v", clerkID, err)
		}
		return id
	}
	fx.adminID = insertUser(fx.adminClerkID, "org-test-admin@example.com", "admin", true)
	fx.memberID = insertUser(fx.memberClerkID, "org-test-member@example.com", "member", false)
	fx.viewerID = insertUser(fx.viewerClerkID, "org-test-viewer@example.com", "viewer", false)

	return fx
}

func TestOrgHandler_List(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodGet, "/api/v1/org/members", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Members []member `json:"members"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 3 {
		t.Fatalf("Members = %d, want 3", len(got.Members))
	}
	byClerkID := map[string]member{}
	for _, m := range got.Members {
		byClerkID[m.ClerkUserID] = m
	}
	if !byClerkID[fx.adminClerkID].CanManageAPIKeys {
		t.Errorf("admin CanManageAPIKeys = false, want true")
	}
	if byClerkID[fx.viewerClerkID].CanManageAPIKeys {
		t.Errorf("viewer CanManageAPIKeys = true, want false")
	}
	if byClerkID[fx.viewerClerkID].Role != "viewer" {
		t.Errorf("viewer role = %q, want viewer", byClerkID[fx.viewerClerkID].Role)
	}
}

func TestOrgHandler_List_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx1 := newTestOrg(t, systemPool)
	fx2 := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	req := authedRequest(t, cfg, fx1.adminClerkID, http.MethodGet, "/api/v1/org/members", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var env apiEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var got struct {
		Members []member `json:"members"`
	}
	_ = json.Unmarshal(env.Data, &got)
	for _, m := range got.Members {
		if m.ClerkUserID == fx2.adminClerkID {
			t.Fatal("org 1's member list leaked org 2's member")
		}
	}
}

func TestOrgHandler_UpdateRole_RequiresOwnerOrAdmin(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	body, _ := json.Marshal(map[string]string{"role": "admin"})
	req := authedRequest(t, cfg, fx.memberClerkID, http.MethodPatch, "/api/v1/org/members/"+fx.viewerID.String()+"/role", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestOrgHandler_UpdateRole_DerivesPermissionsAndSyncsToClerk(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	syncer := &fakeMembershipSyncer{}
	router := testRouter(t, systemPool, appPool, cfg, syncer)

	// Promote the member to admin — permissions should flip to true, and
	// the fake Clerk syncer should see the real clerk_org_id/clerk_user_id.
	body, _ := json.Marshal(map[string]string{"role": "admin"})
	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodPatch, "/api/v1/org/members/"+fx.memberID.String()+"/role", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var role string
	var canManage bool
	if err := systemPool.QueryRow(context.Background(),
		"select role, can_manage_api_keys from users where id = $1", fx.memberID,
	).Scan(&role, &canManage); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || !canManage {
		t.Fatalf("role = %q, can_manage_api_keys = %v, want admin/true", role, canManage)
	}

	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if len(syncer.updateCalls) != 1 {
		t.Fatalf("Clerk UpdateRole calls = %d, want 1", len(syncer.updateCalls))
	}
	call := syncer.updateCalls[0]
	if call.clerkOrgID != fx.clerkOrgID || call.clerkUserID != fx.memberClerkID || call.role != "admin" {
		t.Fatalf("UpdateRole called with %+v, want org=%s user=%s role=admin", call, fx.clerkOrgID, fx.memberClerkID)
	}
}

func TestOrgHandler_UpdateRole_DemotingRevokesPermissions(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	// Demote the admin (fx.adminID) itself via a second, distinct admin —
	// not needed here since the acting admin (fx.adminClerkID) and the
	// target (fx.memberID) are different people; demote member -> viewer.
	body, _ := json.Marshal(map[string]string{"role": "viewer"})
	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodPatch, "/api/v1/org/members/"+fx.memberID.String()+"/role", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var role string
	var canApprove bool
	if err := systemPool.QueryRow(context.Background(),
		"select role, can_approve_workflows from users where id = $1", fx.memberID,
	).Scan(&role, &canApprove); err != nil {
		t.Fatal(err)
	}
	if role != "viewer" || canApprove {
		t.Fatalf("role = %q, can_approve_workflows = %v, want viewer/false", role, canApprove)
	}
}

func TestOrgHandler_UpdateRole_ClerkSyncFailureDoesNotUndoLocalChange(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	syncer := &fakeMembershipSyncer{updateErr: context.DeadlineExceeded}
	router := testRouter(t, systemPool, appPool, cfg, syncer)

	body, _ := json.Marshal(map[string]string{"role": "admin"})
	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodPatch, "/api/v1/org/members/"+fx.memberID.String()+"/role", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a Clerk sync failure must not fail the request)", rec.Code)
	}

	var role string
	if err := systemPool.QueryRow(context.Background(), "select role from users where id = $1", fx.memberID).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Fatalf("role = %q, want admin (local change should stand despite the Clerk sync failure)", role)
	}
}

func TestOrgHandler_UpdateRole_InvalidRoleRejected(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	body, _ := json.Marshal(map[string]string{"role": "superuser"})
	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodPatch, "/api/v1/org/members/"+fx.memberID.String()+"/role", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestOrgHandler_UpdateRole_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx1 := newTestOrg(t, systemPool)
	fx2 := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	body, _ := json.Marshal(map[string]string{"role": "admin"})
	req := authedRequest(t, cfg, fx1.adminClerkID, http.MethodPatch, "/api/v1/org/members/"+fx2.memberID.String()+"/role", body)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (another org's member must not be reachable)", rec.Code)
	}
}

func TestOrgHandler_Remove_RequiresOwnerOrAdmin(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	req := authedRequest(t, cfg, fx.memberClerkID, http.MethodDelete, "/api/v1/org/members/"+fx.viewerID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestOrgHandler_Remove_CannotRemoveSelf(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodDelete, "/api/v1/org/members/"+fx.adminID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestOrgHandler_Remove_Success_CallsClerkThenDeactivatesLocally(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	syncer := &fakeMembershipSyncer{}
	router := testRouter(t, systemPool, appPool, cfg, syncer)

	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodDelete, "/api/v1/org/members/"+fx.viewerID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	syncer.mu.Lock()
	if len(syncer.removeCalls) != 1 || syncer.removeCalls[0].clerkUserID != fx.viewerClerkID {
		syncer.mu.Unlock()
		t.Fatalf("Clerk Remove calls = %+v, want exactly 1 for %s", syncer.removeCalls, fx.viewerClerkID)
	}
	syncer.mu.Unlock()

	var isActive bool
	if err := systemPool.QueryRow(context.Background(), "select is_active from users where id = $1", fx.viewerID).Scan(&isActive); err != nil {
		t.Fatal(err)
	}
	if isActive {
		t.Fatal("removed member's is_active is still true")
	}

	// The real point of deactivation: RequireAuth's own query must now
	// reject this user, exactly like a live revoked session would.
	req2 := authedRequest(t, cfg, fx.viewerClerkID, http.MethodGet, "/api/v1/org/members", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("removed member's own request status = %d, want 401", rec2.Code)
	}
}

// TestOrgHandler_Remove_ClerkFailureLeavesLocalRecordUntouched is the
// resurrection-risk regression test: if Clerk removal fails, the local
// row must NOT be soft-deleted, and Clerk's Remove must have been
// attempted before any local write — see Handler.Remove's doc comment.
func TestOrgHandler_Remove_ClerkFailureLeavesLocalRecordUntouched(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	syncer := &fakeMembershipSyncer{removeErr: context.DeadlineExceeded}
	router := testRouter(t, systemPool, appPool, cfg, syncer)

	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodDelete, "/api/v1/org/members/"+fx.viewerID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (unlike UpdateRole, Remove is not best-effort)", rec.Code)
	}

	var isActive bool
	if err := systemPool.QueryRow(context.Background(), "select is_active from users where id = $1", fx.viewerID).Scan(&isActive); err != nil {
		t.Fatal(err)
	}
	if !isActive {
		t.Fatal("local record was deactivated despite the Clerk removal failing — resurrection-risk regression")
	}
}

func TestOrgHandler_Remove_CrossOrgIsolation(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx1 := newTestOrg(t, systemPool)
	fx2 := newTestOrg(t, systemPool)
	router := testRouter(t, systemPool, appPool, cfg, &fakeMembershipSyncer{})

	req := authedRequest(t, cfg, fx1.adminClerkID, http.MethodDelete, "/api/v1/org/members/"+fx2.memberID.String(), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (another org's member must not be reachable)", rec.Code)
	}
}

func TestOrgHandler_ListInvitations_RequiresOwnerOrAdmin(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouterFull(t, systemPool, appPool, cfg, &fakeMembershipSyncer{}, &fakeInvitationLister{})

	req := authedRequest(t, cfg, fx.memberClerkID, http.MethodGet, "/api/v1/org/invitations", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestOrgHandler_ListInvitations_ReturnsAndConvertsTimestamps(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	expiresAt := int64(1_800_000_000_000) // an arbitrary real millis-since-epoch value
	lister := &fakeInvitationLister{invitations: []Invitation{
		{ID: "orginv_test123", Email: "invitee@example.com", Role: "org:member", Status: "pending", CreatedAt: 1_700_000_000_000, ExpiresAt: &expiresAt},
	}}
	router := testRouterFull(t, systemPool, appPool, cfg, &fakeMembershipSyncer{}, lister)

	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodGet, "/api/v1/org/invitations", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var env apiEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Invitations []invitation `json:"invitations"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Invitations) != 1 {
		t.Fatalf("Invitations = %+v, want 1", got.Invitations)
	}
	inv := got.Invitations[0]
	if inv.Email != "invitee@example.com" || inv.Status != "pending" {
		t.Fatalf("invitation = %+v, want email/status to round-trip", inv)
	}
	if inv.CreatedAt == "" || inv.ExpiresAt == nil {
		t.Fatalf("invitation = %+v, want CreatedAt/ExpiresAt both populated as real timestamps", inv)
	}
}

func TestOrgHandler_RevokeInvitation_RequiresOwnerOrAdmin(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	router := testRouterFull(t, systemPool, appPool, cfg, &fakeMembershipSyncer{}, &fakeInvitationLister{})

	req := authedRequest(t, cfg, fx.memberClerkID, http.MethodDelete, "/api/v1/org/invitations/orginv_test123", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestOrgHandler_RevokeInvitation_Success(t *testing.T) {
	appPool := testAppPool(t)
	systemPool := testSystemPool(t)
	cfg := testConfig(t)
	fx := newTestOrg(t, systemPool)
	lister := &fakeInvitationLister{}
	router := testRouterFull(t, systemPool, appPool, cfg, &fakeMembershipSyncer{}, lister)

	req := authedRequest(t, cfg, fx.adminClerkID, http.MethodDelete, "/api/v1/org/invitations/orginv_test123", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	lister.mu.Lock()
	defer lister.mu.Unlock()
	if len(lister.revokeCalls) != 1 {
		t.Fatalf("Revoke calls = %d, want 1", len(lister.revokeCalls))
	}
	call := lister.revokeCalls[0]
	if call.clerkOrgID != fx.clerkOrgID || call.invitationID != "orginv_test123" || call.requestingClerkUserID != fx.adminClerkID {
		t.Fatalf("Revoke called with %+v, want org=%s invitation=orginv_test123 requester=%s", call, fx.clerkOrgID, fx.adminClerkID)
	}
}
