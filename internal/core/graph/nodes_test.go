package graph

// These are the package's pure, no-DB pieces — toolCallBatchSignature,
// truncateForContext — split from nodes_integration_test.go's real-Postgres
// tests, matching this codebase's unit-vs-integration split everywhere
// else (see CLAUDE.md's "Testing Strategy & CI").

import (
	"strings"
	"testing"

	"github.com/founderstack/api/internal/core/llm"
)

func TestTruncateForContext_ShortTextUnchanged(t *testing.T) {
	short := "just some data"
	if got := truncateForContext(short); got != short {
		t.Fatalf("truncateForContext(%q) = %q, want unchanged", short, got)
	}
}

func TestTruncateForContext_LongTextTruncatedWithNote(t *testing.T) {
	long := strings.Repeat("x", toolResultContextLimit+500)
	got := truncateForContext(long)

	if len(got) <= toolResultContextLimit {
		t.Fatal("expected the truncation note to push length over the raw limit, got exactly the limit or less")
	}
	if !strings.HasPrefix(got, strings.Repeat("x", toolResultContextLimit)) {
		t.Fatal("expected the first toolResultContextLimit bytes to survive untouched")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("got = %q, want it to contain a truncation note", got[len(got)-100:])
	}
}

func TestToolCallBatchSignature_OrderIndependent(t *testing.T) {
	a := []llm.ToolCall{{Name: "fake.get_data", Args: []byte(`{"x":1}`)}, {Name: "fake.list", Args: []byte(`{}`)}}
	b := []llm.ToolCall{{Name: "fake.list", Args: []byte(`{}`)}, {Name: "fake.get_data", Args: []byte(`{"x":1}`)}}

	if toolCallBatchSignature(a) != toolCallBatchSignature(b) {
		t.Fatal("expected the same batch in a different order to produce the same signature")
	}
}

func TestToolCallBatchSignature_DifferentArgsDifferentSignature(t *testing.T) {
	a := []llm.ToolCall{{Name: "fake.get_data", Args: []byte(`{"x":1}`)}}
	b := []llm.ToolCall{{Name: "fake.get_data", Args: []byte(`{"x":2}`)}}

	if toolCallBatchSignature(a) == toolCallBatchSignature(b) {
		t.Fatal("expected different args to produce different signatures")
	}
}
