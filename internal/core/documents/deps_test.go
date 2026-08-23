package documents

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	coherecli "github.com/cohere-ai/cohere-go/v2/client"
	"github.com/cohere-ai/cohere-go/v2/option"
)

// fakeCohereServer returns an httptest server that responds with statuses
// in sequence for the first len(statuses) requests (429/5xx to exercise
// the Cohere SDK's own built-in retrier — see NewCohereEmbedder's doc
// comment for why this package doesn't hand-rolled its own), then a real
// embeddings_floats response — the exact wire shape cohere-go/v2's
// EmbedResponse.UnmarshalJSON discriminates on
// ("response_type": "embeddings_floats").
func fakeCohereServer(t *testing.T, statuses []int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= len(statuses) {
			w.WriteHeader(statuses[calls-1])
			_, _ = w.Write([]byte(`{"message":"synthetic error for test"}`))
			return
		}
		var req struct {
			Texts []string `json:"texts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		embeddings := make([][]float64, len(req.Texts))
		for i := range embeddings {
			embeddings[i] = []float64{0.1, 0.2, 0.3}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response_type": "embeddings_floats",
			"id":            "test-id",
			"texts":         req.Texts,
			"embeddings":    embeddings,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A single 429 falls well within cohere-go's own default retry budget
// (2 attempts) — this proves the SDK's built-in retrier, not any code in
// this package, is what makes a transient rate limit survivable.
func TestCohereEmbedder_SDKRetriesOnRateLimitThenSucceeds(t *testing.T) {
	srv, calls := fakeCohereServer(t, []int{429})
	client := coherecli.NewClient(option.WithToken("test"), option.WithBaseURL(srv.URL))
	embedder := NewCohereEmbedder(client)

	got, err := embedder.Embed(t.Context(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(embeddings) = %d, want 2", len(got))
	}
	if *calls != 2 {
		t.Fatalf("server received %d calls, want 2 (1 failure + 1 retry)", *calls)
	}
}

// A 400 (bad request) is never in cohere-go's shouldRetry set (only
// 429/408/5xx are) — this proves a non-transient error surfaces
// immediately instead of burning through retries pointlessly.
func TestCohereEmbedder_DoesNotRetryOnBadRequest(t *testing.T) {
	srv, calls := fakeCohereServer(t, []int{400})
	client := coherecli.NewClient(option.WithToken("test"), option.WithBaseURL(srv.URL))
	embedder := NewCohereEmbedder(client)

	_, err := embedder.Embed(t.Context(), []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want a non-retryable error")
	}
	if *calls != 1 {
		t.Fatalf("server received %d calls, want 1 (a 400 must not be retried)", *calls)
	}
}

// A sustained rate limit that outlasts the configured attempt budget
// still surfaces as an error rather than hanging or panicking — proves
// WithMaxAttempts actually bounds the retry loop.
func TestCohereEmbedder_GivesUpAfterConfiguredAttempts(t *testing.T) {
	srv, calls := fakeCohereServer(t, []int{429, 429, 429})
	client := coherecli.NewClient(option.WithToken("test"), option.WithBaseURL(srv.URL), option.WithMaxAttempts(2))
	embedder := NewCohereEmbedder(client)

	_, err := embedder.Embed(t.Context(), []string{"a"})
	if err == nil {
		t.Fatal("Embed() error = nil, want an error after exhausting the configured attempts")
	}
	if *calls != 2 {
		t.Fatalf("server received %d calls, want exactly 2 (WithMaxAttempts(2))", *calls)
	}
}
