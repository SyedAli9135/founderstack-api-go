package notify

import (
	"context"
	"encoding/json"
	"log/slog"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/founderstack/api/internal/pkg/secret"
)

// Mirrors push_subscriptions' columns.
type PushSubscription struct {
	Endpoint  string
	P256dhKey string
	AuthKey   string
}

// The JSON body founderstack-web's public/sw.js "push" handler expects.
// ApproveURL/RejectURL are full, ready-to-POST URLs (already carrying
// ?action_token=), not bare tokens — a static sw.js has no build-time
// access to NEXT_PUBLIC_API_URL, so the server builds the complete URL.
type PushPayload struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	ApprovalID string `json:"approval_id"`
	ApproveURL string `json:"approve_url,omitempty"`
	RejectURL  string `json:"reject_url,omitempty"`
}

// SendToSubscription is a logged no-op when either VAPID key is unset.
type WebPushSender struct {
	vapidPublicKey  string
	vapidPrivateKey secret.Value
	subscriber      string
}

func NewWebPushSender(publicKey string, privateKey secret.Value, subscriber string) *WebPushSender {
	return &WebPushSender{vapidPublicKey: publicKey, vapidPrivateKey: privateKey, subscriber: subscriber}
}

func (w *WebPushSender) configured() bool {
	return w.vapidPublicKey != "" && !w.vapidPrivateKey.IsEmpty()
}

func (w *WebPushSender) SendToSubscription(ctx context.Context, sub PushSubscription, payload PushPayload) {
	if !w.configured() {
		slog.Warn("notify: push not sent — WEBPUSH_VAPID keys not configured", "endpoint", sub.Endpoint)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("notify: marshal push payload failed", "err", err)
		return
	}

	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{Auth: sub.AuthKey, P256dh: sub.P256dhKey},
	}, &webpush.Options{
		Subscriber:      w.subscriber,
		VAPIDPublicKey:  w.vapidPublicKey,
		VAPIDPrivateKey: w.vapidPrivateKey.Expose(),
		TTL:             int(ApprovalTTL.Seconds()),
	})
	if err != nil {
		slog.Warn("notify: webpush send failed", "endpoint", sub.Endpoint, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("notify: webpush send failed", "endpoint", sub.Endpoint, "status", resp.StatusCode)
	}
}
