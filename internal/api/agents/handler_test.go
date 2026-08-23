package agents

import (
	"reflect"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Finance Agent", "finance-agent"},
		{"  Leading and trailing  ", "leading-and-trailing"},
		{"Multiple---Hyphens", "multiple-hyphens"},
		{"Emoji 🚀 Agent!!", "emoji-agent"},
		{"already-slug", "already-slug"},
		{"UPPERCASE", "uppercase"},
		{"123 Numbers 456", "123-numbers-456"},
	}
	for _, tc := range cases {
		if got := slugify(tc.name); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestServersFromTools(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"single service", []string{"stripe.get_mrr"}, []string{"stripe"}},
		{
			"dedupes and sorts",
			[]string{"slack.send_message", "stripe.get_mrr", "stripe.list_subscriptions"},
			[]string{"slack", "stripe"},
		},
		{"ignores malformed entries with no dot", []string{"malformed"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serversFromTools(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("serversFromTools(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDerefHelpers(t *testing.T) {
	s := "value"
	if got := derefOr(&s, "fallback"); got != "value" {
		t.Errorf("derefOr(&%q, ...) = %q, want %q", s, got, "value")
	}
	if got := derefOr(nil, "fallback"); got != "fallback" {
		t.Errorf("derefOr(nil, ...) = %q, want fallback", got)
	}
	empty := ""
	if got := derefOr(&empty, "fallback"); got != "fallback" {
		t.Errorf("derefOr(empty, ...) = %q, want fallback (empty string treated as unset)", got)
	}

	n := int32(7)
	if got := derefInt32(&n); got != 7 {
		t.Errorf("derefInt32(&7) = %d, want 7", got)
	}
	if got := derefInt32(nil); got != 0 {
		t.Errorf("derefInt32(nil) = %d, want 0", got)
	}

	if !derefBool(boolPtr(true)) {
		t.Error("derefBool(&true) = false, want true")
	}
	if derefBool(nil) {
		t.Error("derefBool(nil) = true, want false")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestNonNullJSON(t *testing.T) {
	if got := string(nonNullJSON(nil)); got != "null" {
		t.Errorf("nonNullJSON(nil) = %q, want null", got)
	}
	if got := string(nonNullJSON([]byte{})); got != "null" {
		t.Errorf("nonNullJSON(empty) = %q, want null", got)
	}
	if got := string(nonNullJSON([]byte(`{"a":1}`))); got != `{"a":1}` {
		t.Errorf("nonNullJSON(json) = %q, want passthrough", got)
	}
}
