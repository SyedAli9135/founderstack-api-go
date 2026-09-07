package authctx

import (
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

const key = "authctx_user"

// User is the local (not Clerk's) identity for this request. OrgID scopes
// every tenant-scoped DB operation via tenant.WithTx.
type User struct {
	ID                    pgtype.UUID
	OrgID                 pgtype.UUID
	Role                  string
	OrgName               string
	OrgSlug               string
	ClerkOrgID            string
	ClerkUserID           string
	CanManageAPIKeys      bool
	CanManageIntegrations bool
}

// IsOwnerOrAdmin is this app's practical "can administer the org" tier —
// Clerk's own default role for whoever creates an org is "admin" (not a
// distinct "owner"), so admin/owner are treated as equivalent everywhere
// this codebase gates on org-administration (workflow 10's
// canApproveByDefault, workflow 12's document-visibility ACL, and this
// workflow's team-management/agent/API-key guards).
func (u User) IsOwnerOrAdmin() bool {
	return u.Role == "owner" || u.Role == "admin"
}

// CanModifyAgents: workflow 13's "member... cannot modify agents" user
// story — only owner/admin can create/update/delete an agent.
func (u User) CanModifyAgents() bool {
	return u.IsOwnerOrAdmin()
}

// CanTriggerWorkflows: workflow 13's "viewer cannot ... trigger workflows"
// acceptance criterion — deliberately less restrictive than
// CanModifyAgents, since the plan's own "member" user story explicitly
// says a member *can* trigger workflows, just not modify agents.
func (u User) CanTriggerWorkflows() bool {
	return u.Role != "viewer"
}

// CanModifyWorkflows: a workflow's config (which agent, schedule, input
// template) is configuration the same way an agent's own config is — a
// member can trigger one (CanTriggerWorkflows) but shouldn't be able to
// create/edit/delete one, matching CanModifyAgents' boundary exactly. Its
// own method rather than reusing CanModifyAgents so a permission check on
// a workflow reads as being about workflows, not agents.
func (u User) CanModifyWorkflows() bool {
	return u.IsOwnerOrAdmin()
}

// Set stores u on c. Called once, by middleware.RequireAuth.
func Set(c *gin.Context, u User) {
	c.Set(key, u)
}

// FromContext returns the authenticated user, or ok=false if
// middleware.RequireAuth hasn't run on this route.
func FromContext(c *gin.Context) (User, bool) {
	v, exists := c.Get(key)
	if !exists {
		return User{}, false
	}
	u, ok := v.(User)
	return u, ok
}
