package notify

import "testing"

func TestValidPushEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"https://fcm.googleapis.com/fcm/send/abc", true},
		{"https://updates.push.services.mozilla.com/wpush/v2/abc", true},
		{"https://web.push.apple.com/abc", true},
		{"https://db5p.notify.windows.com/?token=abc", true},
		{"http://fcm.googleapis.com/fcm/send/abc", false},   // not https
		{"https://169.254.169.254/latest/meta-data", false}, // cloud metadata
		{"https://localhost/x", false},                      // loopback
		{"https://10.0.0.5/x", false},                       // private range
		{"https://evilgoogleapis.com/x", false},             // suffix without a dot boundary
		{"https://fcm.googleapis.com.evil.test/x", false},   // allowlisted name as a prefix
		{"https://user:pw@fcm.googleapis.com/x", false},     // userinfo trick
		{"https://fcm.googleapis.com:8443/x", false},        // non-standard port
		{"", false},
		{"not a url", false},
	}
	for _, tc := range tests {
		if got := ValidPushEndpoint(tc.endpoint); got != tc.want {
			t.Errorf("ValidPushEndpoint(%q) = %v, want %v", tc.endpoint, got, tc.want)
		}
	}
}
