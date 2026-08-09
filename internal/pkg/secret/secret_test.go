package secret

import (
	"fmt"
	"strings"
	"testing"
)

func TestValue_RedactsInFormatting(t *testing.T) {
	v := Value("sk-ant-super-secret-key")

	for _, format := range []string{"%s", "%v", "%#v"} {
		out := fmt.Sprintf(format, v)
		if strings.Contains(out, "super-secret-key") {
			t.Errorf("fmt %q leaked the secret: %q", format, out)
		}
	}
}

func TestValue_EmptyStringsUnredacted(t *testing.T) {
	// An empty secret isn't sensitive — String() should say so plainly
	// rather than printing "***REDACTED***" for nothing, which would make
	// "is this configured at all?" harder to debug from a log line.
	var v Value
	if got := v.String(); got != "" {
		t.Errorf("String() on empty Value = %q, want \"\"", got)
	}
}

func TestValue_Expose(t *testing.T) {
	const plaintext = "whsec_abc123"
	v := Value(plaintext)
	if got := v.Expose(); got != plaintext {
		t.Errorf("Expose() = %q, want %q", got, plaintext)
	}
}

func TestValue_IsEmpty(t *testing.T) {
	var empty Value
	if !empty.IsEmpty() {
		t.Error("IsEmpty() on zero value = false, want true")
	}
	nonEmpty := Value("x")
	if nonEmpty.IsEmpty() {
		t.Error("IsEmpty() on non-empty value = true, want false")
	}
}
