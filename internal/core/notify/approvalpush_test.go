package notify

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBuildApprovalPush_TokenIsNeverInTheURL(t *testing.T) {
	id := uuid.New()
	p := buildApprovalPush("https://api.example.com", id, "secret-token-value", "write_destructive_or_financial", "refund a payment")

	if p.ActionToken != "secret-token-value" {
		t.Fatalf("ActionToken = %q, want the token carried separately", p.ActionToken)
	}
	for _, u := range []string{p.ApproveURL, p.RejectURL} {
		if strings.Contains(u, "secret-token-value") || strings.Contains(u, "?") {
			t.Fatalf("URL %q carries the token or a query string; it would be logged by proxies", u)
		}
	}
	if p.ApproveURL != "https://api.example.com/api/v1/approvals/"+id.String()+"/approve" {
		t.Fatalf("ApproveURL = %q", p.ApproveURL)
	}
}

func TestBuildApprovalPush_NoTokenMeansNoActionButtons(t *testing.T) {
	p := buildApprovalPush("https://api.example.com", uuid.New(), "", "read", "x")
	if p.ApproveURL != "" || p.RejectURL != "" || p.ActionToken != "" {
		t.Fatalf("payload = %+v, want no action URLs or token without a signing secret", p)
	}
}
