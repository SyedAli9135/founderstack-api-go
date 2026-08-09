// Package authctx stores the authenticated caller (resolved by
// middleware.RequireAuth) on the Gin request context, for handlers to read
// without each one re-deriving it from the JWT.
package authctx

import (
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

const key = "authctx_user"

// User is the local (not Clerk's) identity resolved for this request: our
// own users/organizations rows, already checked active. OrgID is what
// every tenant-scoped DB operation for this request should be run with,
// via tenant.WithTx.
type User struct {
	ID      pgtype.UUID
	OrgID   pgtype.UUID
	Role    string
	OrgName string
	OrgSlug string
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
