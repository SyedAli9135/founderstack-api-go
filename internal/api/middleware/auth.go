package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/jwt"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// JWKCache caches Clerk's JSON Web Keys by kid, per Clerk's own recommendation
// (cache, invalidate only on an unrecognized kid) rather than fetching every
// request. Exported so internal/api/approvals' handler — the other call site
// verifying a Clerk Bearer token outside RequireAuth's gin.HandlerFunc — can
// hold its own instance.
type JWKCache struct {
	mu   sync.RWMutex
	keys map[string]*clerk.JSONWebKey
}

func NewJWKCache() *JWKCache {
	return &JWKCache{keys: make(map[string]*clerk.JSONWebKey)}
}

// get calls fetch only on a cache miss. fetch is a parameter rather than a
// hardcoded call to jwt.GetJSONWebKey so hit/miss behavior is testable
// without a real Clerk API call.
func (c *JWKCache) get(ctx context.Context, keyID string, fetch func(context.Context, string) (*clerk.JSONWebKey, error)) (*clerk.JSONWebKey, error) {
	c.mu.RLock()
	jwk, ok := c.keys[keyID]
	c.mu.RUnlock()
	if ok {
		return jwk, nil
	}

	fetched, err := fetch(ctx, keyID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.keys[keyID] = fetched
	c.mu.Unlock()
	return fetched, nil
}

// RequireAuth verifies the request's Clerk session JWT
// (Authorization: Bearer <token>) and resolves it to a local user + org,
// storing both on the context via authctx for handlers to read. Unlike the
// Python original, which decodes the JWT with signature verification
// turned off, this verifies it for real against Clerk's JWKS.
//
// systemPool must be app_system (BYPASSRLS) — resolving which org a
// brand-new request belongs to has no org context yet to RLS-scope by,
// the same chicken-and-egg case as the webhook's org creation. Handlers
// switch to app_user via tenant.WithTx once identity is resolved.
//
// The token's active-org claim picks which of the person's memberships this
// request acts as — one Clerk user can belong to many orgs (a practice plus
// its client workspaces), each its own users row.
//
// When cfg.DevTokenSecret is set and !cfg.IsProduction(), a token that
// fails real Clerk verification is retried against devtoken.Verify.
func RequireAuth(systemPool *pgxpool.Pool, cfg *config.Config) gin.HandlerFunc {
	cache := NewJWKCache()
	q := dbgen.New(systemPool)

	return func(c *gin.Context) {
		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			response.Fail(c, http.StatusUnauthorized, "MISSING_AUTHORIZATION", "Missing or malformed Authorization header")
			c.Abort()
			return
		}

		ctx := c.Request.Context()

		clerkUserID, clerkOrgID, err := VerifyToken(ctx, cache, cfg, token)
		if err != nil {
			response.Fail(c, http.StatusUnauthorized, "INVALID_TOKEN", "Invalid session token")
			c.Abort()
			return
		}

		user, err := ResolveUser(ctx, q, clerkUserID, clerkOrgID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				response.Fail(c, http.StatusUnauthorized, "USER_NOT_SYNCHRONIZED", "User profile not synchronized")
			} else if errors.Is(err, errOrgNotFound) {
				response.Fail(c, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "Organization not found or inactive")
			} else if errors.Is(err, ErrActiveOrgRequired) {
				response.Fail(c, http.StatusConflict, "ACTIVE_ORGANIZATION_REQUIRED", "You belong to multiple workspaces; select one first")
			} else {
				response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not verify session")
			}
			c.Abort()
			return
		}

		// Detached: a slow write must never add latency to every authenticated
		// request. The query's own WHERE guard (not just this goroutine) is
		// what keeps this to one write per user per 5 minutes, not one per
		// request — see TouchLastLogin's doc comment.
		go func(userID pgtype.UUID) {
			if err := q.TouchLastLogin(context.Background(), userID); err != nil {
				slog.Warn("middleware: touch last_login_at failed", "err", err)
			}
		}(user.ID)

		authctx.Set(c, user)
		c.Next()
	}
}

// RequireIdentity verifies the session token like RequireAuth but resolves
// no org. It exists for the one route that must work when the active org is
// unusable (deactivated, or never synced): listing the person's own
// workspaces so the client can switch into a usable one.
func RequireIdentity(cfg *config.Config) gin.HandlerFunc {
	cache := NewJWKCache()
	return func(c *gin.Context) {
		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			response.Fail(c, http.StatusUnauthorized, "MISSING_AUTHORIZATION", "Missing or malformed Authorization header")
			c.Abort()
			return
		}
		clerkUserID, clerkOrgID, err := VerifyToken(c.Request.Context(), cache, cfg, token)
		if err != nil {
			response.Fail(c, http.StatusUnauthorized, "INVALID_TOKEN", "Invalid session token")
			c.Abort()
			return
		}
		authctx.SetIdentity(c, authctx.Identity{ClerkUserID: clerkUserID, ClerkOrgID: clerkOrgID})
		c.Next()
	}
}

func fetchJWK(ctx context.Context, keyID string) (*clerk.JSONWebKey, error) {
	return jwt.GetJSONWebKey(ctx, &jwt.GetJSONWebKeyParams{KeyID: keyID})
}

// VerifyToken runs the real Clerk verification path, falling back to
// devtoken.Verify, and returns the token's subject (clerk_user_id) and
// active org (clerk_org_id, "" when the session has none) on success.
// Exported so internal/api/approvals' handler can verify the same
// Authorization header for its approve/reject Bearer-token path (the other
// path being a signed action token, not a Clerk JWT — see
// notify.ActionTokenSigner).
func VerifyToken(ctx context.Context, cache *JWKCache, cfg *config.Config, token string) (clerkUserID, clerkOrgID string, err error) {
	clerkUserID, clerkOrgID, err = verifyClerkToken(ctx, cache, token)
	if err != nil {
		return devTokenFallback(cfg, token)
	}
	return clerkUserID, clerkOrgID, nil
}

// verifyClerkToken returns the token's subject and active org. The SDK's
// ActiveOrganizationID already reads both session-token formats (v1's
// "org_id", v2's "o.id").
func verifyClerkToken(ctx context.Context, cache *JWKCache, token string) (string, string, error) {
	unverified, err := jwt.Decode(ctx, &jwt.DecodeParams{Token: token})
	if err != nil {
		return "", "", err
	}
	jwk, err := cache.get(ctx, unverified.KeyID, fetchJWK)
	if err != nil {
		return "", "", err
	}
	claims, err := jwt.Verify(ctx, &jwt.VerifyParams{Token: token, JWK: jwk})
	if err != nil {
		return "", "", err
	}
	return claims.Subject, claims.ActiveOrganizationID, nil
}

// errOrgNotFound distinguishes "no such user" from "user exists but their
// org doesn't" — both are pgx.ErrNoRows from two different queries, so this
// wraps the second one to keep them distinguishable.
var errOrgNotFound = errors.New("middleware: organization not found or inactive")

// ErrActiveOrgRequired: the token names no active org and the person has
// more than one usable membership, so any pick would be a guess.
var ErrActiveOrgRequired = errors.New("middleware: multiple memberships and no active organization")

// ResolveUser resolves (clerkUserID, clerkOrgID) to the local user + org
// row this request acts as. Extracted so internal/api/approvals' handler
// can resolve the same identity. Returns pgx.ErrNoRows when the person has
// no active membership in the resolved org, errOrgNotFound when the org
// itself isn't found/active, ErrActiveOrgRequired when clerkOrgID is empty
// and the person has several active memberships.
func ResolveUser(ctx context.Context, q *dbgen.Queries, clerkUserID, clerkOrgID string) (authctx.User, error) {
	var org dbgen.GetActiveOrganizationByIDRow
	if clerkOrgID != "" {
		row, err := q.GetActiveOrganizationByClerkOrgID(ctx, clerkOrgID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return authctx.User{}, errOrgNotFound
			}
			return authctx.User{}, err
		}
		org = dbgen.GetActiveOrganizationByIDRow(row)
	} else {
		orgID, err := soleMembershipOrgID(ctx, q, clerkUserID)
		if err != nil {
			return authctx.User{}, err
		}
		if org, err = q.GetActiveOrganizationByID(ctx, orgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return authctx.User{}, errOrgNotFound
			}
			return authctx.User{}, err
		}
	}

	user, err := q.GetActiveUserInOrg(ctx, dbgen.GetActiveUserInOrgParams{OrgID: org.ID, ClerkUserID: clerkUserID})
	if err != nil {
		return authctx.User{}, err
	}
	return authctx.User{
		ID:                    user.ID,
		OrgID:                 user.OrgID,
		Role:                  user.Role,
		OrgName:               org.Name,
		OrgSlug:               org.Slug,
		ClerkOrgID:            org.ClerkOrgID,
		ClerkUserID:           clerkUserID,
		OrganizationType:      org.OrganizationType,
		ParentPracticeID:      org.ParentPracticeID,
		CanManageAPIKeys:      user.CanManageApiKeys != nil && *user.CanManageApiKeys,
		CanManageIntegrations: user.CanManageIntegrations != nil && *user.CanManageIntegrations,
	}, nil
}

// soleMembershipOrgID keeps a token with no active-org claim working for
// the common single-org case (dev tokens, sessions predating an org
// switch) without ever guessing between several orgs.
func soleMembershipOrgID(ctx context.Context, q *dbgen.Queries, clerkUserID string) (pgtype.UUID, error) {
	rows, err := q.ListActiveMembershipsByClerkUserID(ctx, clerkUserID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	var active []pgtype.UUID
	for _, r := range rows {
		if r.OrgIsActive {
			active = append(active, r.OrgID)
		}
	}
	switch {
	case len(active) == 1:
		return active[0], nil
	case len(active) > 1:
		return pgtype.UUID{}, ErrActiveOrgRequired
	case len(rows) > 0:
		return pgtype.UUID{}, errOrgNotFound
	default:
		return pgtype.UUID{}, pgx.ErrNoRows
	}
}

// devTokenFallback only does anything when cfg.DevTokenSecret is configured
// and the process isn't production — every real environment should leave
// DEV_TOKEN_SECRET unset.
func devTokenFallback(cfg *config.Config, token string) (string, string, error) {
	if cfg.IsProduction() || cfg.DevTokenSecret.IsEmpty() {
		return "", "", errors.New("dev token fallback not enabled")
	}
	return devtoken.Verify(cfg.DevTokenSecret.Expose(), token)
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimPrefix(header, prefix)
}
