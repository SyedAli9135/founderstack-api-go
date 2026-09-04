package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/jwt"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
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

		clerkUserID, err := VerifyToken(ctx, cache, cfg, token)
		if err != nil {
			response.Fail(c, http.StatusUnauthorized, "INVALID_TOKEN", "Invalid session token")
			c.Abort()
			return
		}

		user, err := ResolveUser(ctx, q, clerkUserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				response.Fail(c, http.StatusUnauthorized, "USER_NOT_SYNCHRONIZED", "User profile not synchronized")
			} else if errors.Is(err, errOrgNotFound) {
				response.Fail(c, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "Organization not found or inactive")
			} else {
				response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not verify session")
			}
			c.Abort()
			return
		}

		authctx.Set(c, user)
		c.Next()
	}
}

func fetchJWK(ctx context.Context, keyID string) (*clerk.JSONWebKey, error) {
	return jwt.GetJSONWebKey(ctx, &jwt.GetJSONWebKeyParams{KeyID: keyID})
}

// VerifyToken runs the real Clerk verification path, falling back to
// devtoken.Verify, and returns the token's subject (clerk_user_id) on
// success. Exported so internal/api/approvals' handler can verify the same
// Authorization header for its approve/reject Bearer-token path (the other
// path being a signed action token, not a Clerk JWT — see
// notify.ActionTokenSigner).
func VerifyToken(ctx context.Context, cache *JWKCache, cfg *config.Config, token string) (string, error) {
	clerkUserID, err := verifyClerkToken(ctx, cache, token)
	if err != nil {
		return devTokenFallback(cfg, token)
	}
	return clerkUserID, nil
}

// verifyClerkToken returns the token's subject (a clerk_user_id) on success.
func verifyClerkToken(ctx context.Context, cache *JWKCache, token string) (string, error) {
	unverified, err := jwt.Decode(ctx, &jwt.DecodeParams{Token: token})
	if err != nil {
		return "", err
	}
	jwk, err := cache.get(ctx, unverified.KeyID, fetchJWK)
	if err != nil {
		return "", err
	}
	claims, err := jwt.Verify(ctx, &jwt.VerifyParams{Token: token, JWK: jwk})
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

// errOrgNotFound distinguishes "no such user" from "user exists but their
// org doesn't" — both are pgx.ErrNoRows from two different queries, so this
// wraps the second one to keep them distinguishable.
var errOrgNotFound = errors.New("middleware: organization not found or inactive")

// ResolveUser looks up clerkUserID's local user + org row. Extracted so
// internal/api/approvals' handler can resolve the same identity without
// duplicating these two dbgen calls. Returns pgx.ErrNoRows when the user
// itself isn't found/inactive, errOrgNotFound when the user's org isn't.
func ResolveUser(ctx context.Context, q *dbgen.Queries, clerkUserID string) (authctx.User, error) {
	user, err := q.GetActiveUserByClerkUserID(ctx, clerkUserID)
	if err != nil {
		return authctx.User{}, err
	}
	org, err := q.GetActiveOrganizationByID(ctx, user.OrgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authctx.User{}, errOrgNotFound
		}
		return authctx.User{}, err
	}
	return authctx.User{
		ID:      user.ID,
		OrgID:   user.OrgID,
		Role:    user.Role,
		OrgName: org.Name,
		OrgSlug: org.Slug,
	}, nil
}

// devTokenFallback only does anything when cfg.DevTokenSecret is configured
// and the process isn't production — every real environment should leave
// DEV_TOKEN_SECRET unset.
func devTokenFallback(cfg *config.Config, token string) (string, error) {
	if cfg.IsProduction() || cfg.DevTokenSecret.IsEmpty() {
		return "", errors.New("dev token fallback not enabled")
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
