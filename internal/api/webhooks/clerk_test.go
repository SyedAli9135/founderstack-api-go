package webhooks

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHandle_MisconfiguredSecret needs no database: Handle checks h.secret
// before it ever touches h.db, so a nil pool is safe here — this is the one
// error path cheap enough to be a plain unit test rather than living in
// clerk_integration_test.go with the rest of Handle's error paths.
func TestHandle_MisconfiguredSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewClerkHandler(nil, "").Register(r.Group("/api/webhooks"))

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/clerk", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("WEBHOOK_MISCONFIGURED")) {
		t.Fatalf("body = %s, want it to contain WEBHOOK_MISCONFIGURED", rec.Body.String())
	}
}

func TestNormalizeRole(t *testing.T) {
	tests := []struct {
		name string
		role string
		want string
	}{
		{"namespaced admin", "org:admin", "admin"},
		{"namespaced member", "org:member", "member"},
		{"already bare", "admin", "admin"},
		{"empty string", "", ""},
		{"multiple colons uses last segment", "a:b:c", "c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRole(tc.role); got != tc.want {
				t.Errorf("normalizeRole(%q) = %q, want %q", tc.role, got, tc.want)
			}
		})
	}
}

func TestNilIfEmpty(t *testing.T) {
	if got := nilIfEmpty(""); got != nil {
		t.Errorf("nilIfEmpty(\"\") = %q, want nil", *got)
	}
	got := nilIfEmpty("Ada Lovelace")
	if got == nil {
		t.Fatal("nilIfEmpty(\"Ada Lovelace\") = nil, want non-nil")
	}
	if *got != "Ada Lovelace" {
		t.Errorf("nilIfEmpty(\"Ada Lovelace\") = %q, want \"Ada Lovelace\"", *got)
	}
}
