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

// stateTTL bounds how long a founder has to complete the provider's
// authorization page before the callback 400s.
const stateTTL = 10 * time.Minute

var (
	// ErrStateInvalid: malformed or forged (HMAC mismatch / rotated secret).
	ErrStateInvalid = errors.New("integrations: malformed or forged oauth state")
	// ErrStateExpired: signature valid but no Redis entry — TTL'd out or
	// already consumed (one-time use).
	ErrStateExpired = errors.New("integrations: oauth state missing or expired")
)

// StateManager mints/verifies the CSRF `state` param. A callback carries no
// JWT (the provider redirects the browser directly), so org_id/service must
// be recovered from the state — Redis holds them; the HMAC only proves the
// state wasn't tampered with.
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

// Generate mints a one-time state token for orgID/service, recorded in
// Redis under a fresh random nonce.
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

// Verify checks the signature first (a forged state costs only CPU, no
// Redis call), then deletes the entry on lookup so a replay 2nd-attempt
// fails with ErrStateExpired.
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
	// Best-effort delete: the entry still expires via its own TTL either way.
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
