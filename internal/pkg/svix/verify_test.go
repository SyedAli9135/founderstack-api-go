package svix

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"
)

// sign reimplements the signing side directly (not by calling Verify) so
// the test doesn't just check the function against itself.
func sign(t *testing.T, secretBytes []byte, id, timestamp string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(id + "." + timestamp + "." + string(body)))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func testSecret(t *testing.T) (string, []byte) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(raw), raw
}

func TestVerify(t *testing.T) {
	secret, secretBytes := testSecret(t)
	id := "msg_test123"
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"type":"organization.created","data":{"id":"org_123"}}`)
	validSig := sign(t, secretBytes, id, timestamp, body)

	otherSecret, _ := testSecret(t)

	tests := []struct {
		name      string
		secret    string
		id        string
		timestamp string
		signature string
		body      []byte
		wantErr   error // nil means "any error is fine, just must fail" when wantFail is set without a specific error
		wantFail  bool
	}{
		{
			name:      "valid signature",
			secret:    secret,
			id:        id,
			timestamp: timestamp,
			signature: validSig,
			body:      body,
			wantFail:  false,
		},
		{
			name:      "valid signature among multiple (secret rotation)",
			secret:    secret,
			id:        id,
			timestamp: timestamp,
			signature: "v1,bm90LXRoZS1yaWdodC1zaWc= " + validSig,
			body:      body,
			wantFail:  false,
		},
		{
			name:      "wrong secret",
			secret:    otherSecret,
			id:        id,
			timestamp: timestamp,
			signature: validSig,
			body:      body,
			wantErr:   ErrInvalidSignature,
			wantFail:  true,
		},
		{
			name:      "tampered body",
			secret:    secret,
			id:        id,
			timestamp: timestamp,
			signature: validSig,
			body:      []byte(`{"type":"organization.created","data":{"id":"org_EVIL"}}`),
			wantErr:   ErrInvalidSignature,
			wantFail:  true,
		},
		{
			name:      "tampered id",
			secret:    secret,
			id:        "msg_different",
			timestamp: timestamp,
			signature: validSig,
			body:      body,
			wantErr:   ErrInvalidSignature,
			wantFail:  true,
		},
		{
			name:      "missing svix-id",
			secret:    secret,
			id:        "",
			timestamp: timestamp,
			signature: validSig,
			body:      body,
			wantErr:   ErrMissingHeaders,
			wantFail:  true,
		},
		{
			name:      "missing svix-signature",
			secret:    secret,
			id:        id,
			timestamp: timestamp,
			signature: "",
			body:      body,
			wantErr:   ErrMissingHeaders,
			wantFail:  true,
		},
		{
			name:      "malformed signature header (no v1 prefix)",
			secret:    secret,
			id:        id,
			timestamp: timestamp,
			signature: "not-a-valid-signature",
			body:      body,
			wantErr:   ErrInvalidSignature,
			wantFail:  true,
		},
		{
			name:      "stale timestamp (replay attempt)",
			secret:    secret,
			id:        id,
			timestamp: strconv.FormatInt(time.Now().Add(-1*time.Hour).Unix(), 10),
			signature: sign(t, secretBytes, id, strconv.FormatInt(time.Now().Add(-1*time.Hour).Unix(), 10), body),
			body:      body,
			wantErr:   ErrInvalidTimestamp,
			wantFail:  true,
		},
		{
			name:      "timestamp too far in the future",
			secret:    secret,
			id:        id,
			timestamp: strconv.FormatInt(time.Now().Add(1*time.Hour).Unix(), 10),
			signature: sign(t, secretBytes, id, strconv.FormatInt(time.Now().Add(1*time.Hour).Unix(), 10), body),
			body:      body,
			wantErr:   ErrInvalidTimestamp,
			wantFail:  true,
		},
		{
			name:      "non-numeric timestamp",
			secret:    secret,
			id:        id,
			timestamp: "not-a-number",
			signature: validSig,
			body:      body,
			wantErr:   ErrInvalidTimestamp,
			wantFail:  true,
		},
		{
			name:      "malformed secret (not valid base64 after prefix)",
			secret:    "whsec_not-valid-base64!!!",
			id:        id,
			timestamp: timestamp,
			signature: validSig,
			body:      body,
			wantErr:   ErrInvalidSecret,
			wantFail:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.secret, tc.id, tc.timestamp, tc.signature, tc.body)
			if tc.wantFail && err == nil {
				t.Fatalf("Verify() succeeded, want failure")
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("Verify() = %v, want success", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify() error = %v, want error wrapping %v", err, tc.wantErr)
			}
		})
	}
}
