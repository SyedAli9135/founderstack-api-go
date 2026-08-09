// Package svix verifies Svix-format webhook signatures (the scheme Clerk
// uses for its webhooks) without pulling in the svix-webhooks SDK — the
// algorithm is short enough to own directly and test in isolation.
//
// https://docs.svix.com/receiving/verifying-payloads/how-manual
package svix

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrMissingHeaders   = errors.New("svix: missing id, timestamp, or signature")
	ErrInvalidSecret    = errors.New("svix: malformed webhook secret")
	ErrInvalidTimestamp = errors.New("svix: missing, malformed, or stale timestamp")
	ErrInvalidSignature = errors.New("svix: no signature matched")
)

// Tolerance is how far a webhook's timestamp may drift from now before it's
// rejected as stale — guards against replaying a captured request. Svix's
// own default is 5 minutes.
const Tolerance = 5 * time.Minute

// Verify checks a webhook's signature against secret (Clerk's
// "whsec_..."-format CLERK_WEBHOOK_SECRET). id, timestamp, and signature
// come from the svix-id / svix-timestamp / svix-signature request headers;
// body is the raw, unparsed request body — signature verification must run
// against the exact bytes that were signed, before any JSON decoding.
func Verify(secret, id, timestamp, signature string, body []byte) error {
	if id == "" || timestamp == "" || signature == "" {
		return ErrMissingHeaders
	}

	secretBytes, err := decodeSecret(secret)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSecret, err)
	}

	if err := checkTimestamp(timestamp); err != nil {
		return err
	}

	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(id + "." + timestamp + "." + string(body)))
	expected := mac.Sum(nil)

	// svix-signature holds one or more space-separated "v{version},{sig}"
	// values (multiple when a secret was recently rotated) — any match is
	// sufficient.
	for _, candidate := range strings.Fields(signature) {
		version, sig, ok := strings.Cut(candidate, ",")
		if !ok || version != "v1" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(decoded, expected) {
			return nil
		}
	}
	return ErrInvalidSignature
}

func decodeSecret(secret string) ([]byte, error) {
	secret = strings.TrimPrefix(secret, "whsec_")
	return base64.StdEncoding.DecodeString(secret)
}

func checkTimestamp(timestamp string) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTimestamp, err)
	}
	age := time.Since(time.Unix(seconds, 0))
	if age > Tolerance || age < -Tolerance {
		return ErrInvalidTimestamp
	}
	return nil
}
