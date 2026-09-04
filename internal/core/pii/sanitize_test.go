package pii

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitize_KeyNameMatch(t *testing.T) {
	in := map[string]any{
		"customer_id": "cus_123",
		"api_key":     "sk_live_abcdef1234567890",
		"password":    "hunter2",
		"nested":      map[string]any{"access_token": "xyz", "note": "fine"},
	}
	out := Sanitize(in).(map[string]any)

	if out["customer_id"] != "cus_123" {
		t.Errorf("non-sensitive key was mutated: %v", out["customer_id"])
	}
	if out["api_key"] != Redacted || out["password"] != Redacted {
		t.Errorf("sensitive keys not redacted: %+v", out)
	}
	nested := out["nested"].(map[string]any)
	if nested["access_token"] != Redacted {
		t.Errorf("nested sensitive key not redacted: %+v", nested)
	}
	if nested["note"] != "fine" {
		t.Errorf("nested non-sensitive key was mutated: %v", nested["note"])
	}
}

func TestSanitize_ValuePatternMatch(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"email", "contact me at founder@example.com please"},
		{"ssn", "SSN on file: 123-45-6789"},
		{"stripe key", "using key sk_live_4242424242424242abcd in the request"},
		{"slack token", "token xoxb-test-fixture-not-a-real-token"},
		{"github pat", "auth with ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ01234"},
		{"bearer", "Authorization: Bearer abc123.def456.ghi789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.in).(string)
			if !strings.Contains(got, Redacted) {
				t.Errorf("Sanitize(%q) = %q, want it to contain %q", tc.in, got, Redacted)
			}
		})
	}
}

func TestSanitize_LeavesOrdinaryDataAlone(t *testing.T) {
	in := map[string]any{"channel": "#general", "count": float64(3), "ok": true}
	out := Sanitize(in).(map[string]any)
	if out["channel"] != "#general" || out["count"] != float64(3) || out["ok"] != true {
		t.Errorf("ordinary data was mutated: %+v", out)
	}
}

func TestSanitizeJSON(t *testing.T) {
	raw := []byte(`{"email":"founder@example.com","api_key":"sk_live_deadbeef12345678","channel":"#general"}`)
	got := SanitizeJSON(raw)

	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("SanitizeJSON produced invalid JSON: %v", err)
	}
	if decoded["api_key"] != Redacted {
		t.Errorf("api_key not redacted: %+v", decoded)
	}
	if decoded["email"] != Redacted {
		t.Errorf("email value pattern not redacted: %+v", decoded)
	}
	if decoded["channel"] != "#general" {
		t.Errorf("ordinary field mutated: %+v", decoded)
	}
}

func TestSanitizeJSON_NonObjectFallsBackToStringScan(t *testing.T) {
	raw := []byte(`"error: invalid token xoxb-test-fixture-not-a-real-token"`)
	got := SanitizeJSON(raw)

	var decoded string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("SanitizeJSON produced invalid JSON: %v", err)
	}
	if !strings.Contains(decoded, Redacted) {
		t.Errorf("SanitizeJSON(%q) = %q, want token redacted", raw, decoded)
	}
}

func TestSanitizeJSON_Empty(t *testing.T) {
	if got := SanitizeJSON(nil); got != nil {
		t.Errorf("SanitizeJSON(nil) = %v, want nil", got)
	}
}
