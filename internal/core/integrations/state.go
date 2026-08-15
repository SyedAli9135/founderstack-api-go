package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// stateTTL bounds how long a founder has between clicking "Connect" and
// completing the provider's authorization page before the state (and the
// org_id/service it carries) is forgotten and the callback 400s.
const stateTTL = 10 * time.Minute

var (
	// ErrStateInvalid means the state string is malformed or its HMAC
	// signature doesn't match — either a forged/tampered value, or one
	// signed under a different (e.g. rotated) OAUTH_STATE_SECRET.
	ErrStateInvalid = errors.New("integrations: malformed or forged oauth state")
	// ErrStateExpired means the signature checked out but no matching
	// entry exists in Redis anymore — it already expired (>10 min) or was
	// already consumed by an earlier callback (one-time use).
	ErrStateExpired = errors.New("integrations: oauth state missing or expired")
)

// StateManager mints and verifies the CSRF-protecting `state` parameter
// used on every OAuth authorize URL. A callback request carries no JWT
// (the provider redirects the browser here directly), so org_id and
// service must be recoverable from the state itself — Redis is the
// source of truth for that; the HMAC signature only proves the state
// wasn't tampered with in transit, not what it means.
type StateManager struct {
	rdb    *redis.Client
	secret string
}

// NewStateManager builds a StateManager. secret is the decoded
// OAUTH_STATE_SECRET.
func NewStateManager(rdb *redis.Client, secret string) *StateManager {
	return &StateManager{rdb: rdb, secret: secret}
}

type stateValue struct {
	OrgID   string `json:"org_id"`
	Service string `json:"service"`
}

// Generate mints a one-time state token for orgID/service and records
// them in Redis under it, keyed by a fresh random nonce.
func (s *StateManager) Generate(ctx context.Context, orgID, service string) (string, error) {
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("integrations: generate state nonce: %w", err)
	}
	nonce := base64.URLEncoding.EncodeToString(nonceBytes)

	payload, err := json.Marshal(stateValue{OrgID: orgID, Service: service})
	if err != nil {
		return "", fmt.Errorf("integrations: marshal state value: %w", err)
	}
	if err := s.rdb.Set(ctx, stateKey(nonce), payload, stateTTL).Err(); err != nil {
		return "", fmt.Errorf("integrations: store oauth state: %w", err)
	}

	return nonce + "." + s.sign(nonce), nil
}

// Verify checks state's signature, then looks up and immediately deletes
// its Redis entry (one-time use — a replayed callback with the same
// state fails with ErrStateExpired on its second attempt, not just its
// first). Signature verification runs before any Redis call so a
// garbage/forged state costs nothing but CPU.
func (s *StateManager) Verify(ctx context.Context, state string) (orgID, service string, err error) {
	nonce, sig, ok := strings.Cut(state, ".")
	if !ok || nonce == "" || sig == "" {
		return "", "", ErrStateInvalid
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(nonce))) {
		return "", "", ErrStateInvalid
	}

	key := stateKey(nonce)
	payload, err := s.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", "", ErrStateExpired
		}
		return "", "", fmt.Errorf("integrations: fetch oauth state: %w", err)
	}
	// Best-effort: if the delete itself fails, the entry still expires via
	// its own TTL, and the HMAC check above already stops it being reused
	// to forge a *different* request — this isn't the security boundary.
	_ = s.rdb.Del(ctx, key).Err()

	var v stateValue
	if err := json.Unmarshal(payload, &v); err != nil {
		return "", "", fmt.Errorf("integrations: unmarshal state value: %w", err)
	}
	return v.OrgID, v.Service, nil
}

func (s *StateManager) sign(nonce string) string {
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write([]byte(nonce))
	return base64.URLEncoding.EncodeToString(mac.Sum(nil))
}

func stateKey(nonce string) string {
	return "oauth_state:" + nonce
}
