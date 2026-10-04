package integrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"golang.org/x/oauth2"
)

func retrieveErr(status int, code string) error {
	return fmt.Errorf("providers: refresh access token: %w", &oauth2.RetrieveError{Response: &http.Response{StatusCode: status}, ErrorCode: code})
}

func TestIsPermanentRefreshError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"invalid_grant (revoked refresh token)", retrieveErr(400, "invalid_grant"), true},
		{"invalid_client", retrieveErr(401, "invalid_client"), true},
		{"a 4xx in a provider's own shape", retrieveErr(403, ""), true},
		{"a 500 from the provider", retrieveErr(500, ""), false},
		{"a 503 even with an error code", retrieveErr(503, "temporarily_unavailable"), false},
		{"rate limited", retrieveErr(429, ""), false},
		{"a network failure", errors.New("dial tcp: connection refused"), false},
		{"a timeout", context.DeadlineExceeded, false},
		{"nothing", nil, false},
	} {
		if got := IsPermanentRefreshError(tc.err); got != tc.want {
			t.Errorf("%s: IsPermanentRefreshError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
