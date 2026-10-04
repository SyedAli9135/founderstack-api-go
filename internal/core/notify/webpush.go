package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/founderstack/api/internal/pkg/secret"
)

// pushServiceHosts are the browser vendors' push services; a subscription
// endpoint is a URL the server will POST to, so anything else (an internal
// address, a cloud metadata service) would be a server-side request forgery.
var pushServiceHosts = []string{
	"googleapis.com",            // Chrome, Edge, Brave, Samsung (FCM)
	"push.services.mozilla.com", // Firefox
	"push.apple.com",            // Safari
	"notify.windows.com",        // legacy Edge / WNS
}

// ValidPushEndpoint reports whether raw is an https URL on a known push
// service. Callers check it both when a subscription is saved and when a
// push is sent, so rows stored before this check existed are covered too.
func ValidPushEndpoint(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range pushServiceHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// Mirrors push_subscriptions' columns.
type PushSubscription struct {
	Endpoint  string
	P256dhKey string
	AuthKey   string
}

// The JSON body founderstack-web's public/sw.js "push" handler expects.
// ApproveURL/RejectURL are full, ready-to-POST URLs — a static sw.js has no
// build-time access to NEXT_PUBLIC_API_URL, so the server builds the complete
// URL — and ActionToken is sent in the X-Action-Token header, never the URL.
type PushPayload struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	ApprovalID string `json:"approval_id"`
	ApproveURL string `json:"approve_url,omitempty"`
	RejectURL  string `json:"reject_url,omitempty"`
	// ActionToken authorizes the approve/reject URLs above, sent as a header.
	ActionToken string `json:"action_token,omitempty"`
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
	if !ValidPushEndpoint(sub.Endpoint) {
		slog.Warn("notify: push not sent — endpoint is not a known push service")
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
