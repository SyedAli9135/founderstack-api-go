package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These test the request-shape logic (headers, status-code-to-error
// mapping) against fake httptest servers, never the real provider APIs —
// same "flakiness/cost liability" reasoning llm_test.go's doc comment
// gives for not covering verifyAnthropic's network path directly.

func TestVerifyOpenAICompatible_OKMeansValid(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := verifyOpenAICompatible(srv.URL)(context.Background(), "sk-test-key")
	if err != nil {
		t.Fatalf("verify() error = %v, want nil", err)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer sk-test-key")
	}
}

func TestVerifyOpenAICompatible_UnauthorizedIsKeyRejected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		err := verifyOpenAICompatible(srv.URL)(context.Background(), "sk-bad-key")
		srv.Close()
		if !errors.Is(err, ErrKeyRejected) {
			t.Errorf("status %d: verify() error = %v, want ErrKeyRejected", status, err)
		}
	}
}

func TestVerifyOpenAICompatible_ServerErrorIsValidationUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := verifyOpenAICompatible(srv.URL)(context.Background(), "sk-whatever")
	if !errors.Is(err, ErrValidationUnavailable) {
		t.Fatalf("verify() error = %v, want ErrValidationUnavailable", err)
	}
}

func TestVerifyOpenAICompatible_UnreachableServerIsValidationUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := srv.URL
	srv.Close() // closed before the request, so the connection genuinely fails

	err := verifyOpenAICompatible(unreachableURL)(context.Background(), "sk-whatever")
	if !errors.Is(err, ErrValidationUnavailable) {
		t.Fatalf("verify() error = %v, want ErrValidationUnavailable", err)
	}
}

func TestVerifyGemini_OKMeansValid(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Cleanup(swapGeminiURL(srv.URL))

	if err := verifyGemini(context.Background(), "AIzaTestKey"); err != nil {
		t.Fatalf("verifyGemini() error = %v, want nil", err)
	}
	if gotQuery != "key=AIzaTestKey" {
		t.Fatalf("query = %q, want %q", gotQuery, "key=AIzaTestKey")
	}
}

func TestVerifyGemini_BadRequestIsKeyRejected(t *testing.T) {
	// Google returns 400, not 401, for a malformed/invalid API key —
	// distinct from the OpenAI-compatible providers' 401/403 convention.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Cleanup(swapGeminiURL(srv.URL))

	err := verifyGemini(context.Background(), "AIzaBadKey")
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("verifyGemini() error = %v, want ErrKeyRejected", err)
	}
}

func TestVerifyGemini_ServerErrorIsValidationUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Cleanup(swapGeminiURL(srv.URL))

	err := verifyGemini(context.Background(), "AIzaWhatever")
	if !errors.Is(err, ErrValidationUnavailable) {
		t.Fatalf("verifyGemini() error = %v, want ErrValidationUnavailable", err)
	}
}

// swapGeminiURL points geminiModelsURL at a fake server for the duration
// of one test and returns a restore func for t.Cleanup.
func swapGeminiURL(fakeURL string) func() {
	original := geminiModelsURL
	geminiModelsURL = fakeURL
	return func() { geminiModelsURL = original }
}
