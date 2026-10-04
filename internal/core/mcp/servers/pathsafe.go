package servers

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Tool arguments are chosen by the model, which reads untrusted content, so
// one interpolated into a URL path must not be able to climb to a different
// endpoint on the same host (where the org's credential would still apply).
var pathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func pathSegment(field, v string) error {
	if !pathSegmentPattern.MatchString(v) || v == "." || v == ".." {
		return fmt.Errorf("%s contains characters that aren't valid in an identifier", field)
	}
	return nil
}

// checkDiscordWebhookURL pins the stored webhook to Discord, so a bad value
// in the connection can't turn send_message into a request to another host.
// A var so tests can aim the happy path at a local server.
var checkDiscordWebhookURL = func(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.HasPrefix(u.Path, "/api/webhooks/") {
		return fmt.Errorf("discord: stored webhook URL is not a Discord webhook — reconnect Discord")
	}
	host := strings.ToLower(u.Hostname())
	if host != "discord.com" && host != "discordapp.com" && !strings.HasSuffix(host, ".discord.com") && !strings.HasSuffix(host, ".discordapp.com") {
		return fmt.Errorf("discord: stored webhook URL is not a Discord webhook — reconnect Discord")
	}
	return nil
}
