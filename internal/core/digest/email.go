package digest

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
)

//go:embed templates/digest_email.html
var emailTemplateSource string

var emailTemplate = template.Must(template.New("digest_email").Parse(emailTemplateSource))

// emailView is what the template actually renders against -- Payload
// plus the one field (UnsubscribeURL) that's a property of the send, not
// of the org's data.
type emailView struct {
	Payload
	UnsubscribeURL string
}

// RenderEmail returns both a text and an HTML body from the same
// Payload, matching brevoSender's optional-HTML Send signature.
func RenderEmail(p *Payload, unsubscribeURL string) (subject, textBody, htmlBody string, err error) {
	subject = fmt.Sprintf("Your FounderStack digest for %s", p.Date)

	var buf bytes.Buffer
	if err := emailTemplate.Execute(&buf, emailView{Payload: *p, UnsubscribeURL: unsubscribeURL}); err != nil {
		return "", "", "", fmt.Errorf("digest: render html body: %w", err)
	}
	return subject, renderText(p, unsubscribeURL), buf.String(), nil
}

func renderText(p *Payload, unsubscribeURL string) string {
	if !p.HadActivity {
		return fmt.Sprintf(
			"Your FounderStack digest for %s\n\nNo runs yesterday — all quiet.\n\nPending approvals: %d\n\nUnsubscribe: %s\n",
			p.Date, p.PendingApprovals, unsubscribeURL,
		)
	}

	topAgentLine := ""
	if p.TopAgentName != "" {
		topAgentLine = fmt.Sprintf("Top agent: %s (%d runs)\n", p.TopAgentName, p.TopAgentRuns)
	}
	return fmt.Sprintf(
		"Your FounderStack digest for %s\n\n"+
			"Runs completed: %d (%d succeeded, %d failed)\n"+
			"Hours saved: %.1f\n"+
			"Cost incurred: $%.2f\n"+
			"Pending approvals: %d\n"+
			"%s\n"+
			"Unsubscribe: %s\n",
		p.Date, p.TotalRuns, p.SuccessfulRuns, p.FailedRuns, p.HoursSaved, p.CostUSD, p.PendingApprovals, topAgentLine, unsubscribeURL,
	)
}
