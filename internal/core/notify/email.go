package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/founderstack/api/internal/pkg/secret"
)

type EmailSender interface {
	Send(ctx context.Context, toEmail, subject, textBody string) error
}

// noopSender degrades gracefully when BREVO_API_KEY/BREVO_FROM_EMAIL are
// unset, so the app boots fine with email simply not configured yet.
func NewEmailSender(apiKey secret.Value, fromEmail string) EmailSender {
	if apiKey.IsEmpty() || fromEmail == "" {
		return noopSender{}
	}
	return &brevoSender{
		apiKey:    apiKey,
		fromEmail: fromEmail,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

type noopSender struct{}

func (noopSender) Send(ctx context.Context, toEmail, subject, textBody string) error {
	slog.Warn("notify: email not sent — BREVO_API_KEY/BREVO_FROM_EMAIL not configured", "to", toEmail, "subject", subject)
	return nil
}

// Chosen over SendGrid: SendGrid's free tier is now a 60-day trial only;
// Brevo has a genuinely free-forever tier (300/day).
type brevoSender struct {
	apiKey    secret.Value
	fromEmail string
	client    *http.Client
}

const brevoEndpoint = "https://api.brevo.com/v3/smtp/email"

type brevoEmailAddress struct {
	Email string `json:"email"`
}

type brevoSendRequest struct {
	Sender      brevoEmailAddress   `json:"sender"`
	To          []brevoEmailAddress `json:"to"`
	Subject     string              `json:"subject"`
	TextContent string              `json:"textContent"`
}

func (s *brevoSender) Send(ctx context.Context, toEmail, subject, textBody string) error {
	body, err := json.Marshal(brevoSendRequest{
		Sender:      brevoEmailAddress{Email: s.fromEmail},
		To:          []brevoEmailAddress{{Email: toEmail}},
		Subject:     subject,
		TextContent: textBody,
	})
	if err != nil {
		return fmt.Errorf("notify: marshal brevo request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brevoEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build brevo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("api-key", s.apiKey.Expose())

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: brevo request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: brevo send failed, status %d", resp.StatusCode)
	}
	return nil
}
