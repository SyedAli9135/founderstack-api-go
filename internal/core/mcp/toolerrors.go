package mcp

import (
	"errors"
	"fmt"
)

// ErrToolTerminal means a tool's third-party API call failed in a way
// retrying won't fix — a bad request, rejected auth/permissions, a
// missing resource, a business-logic conflict. ErrToolRetryable means
// the failure looks transient — rate limited, a server error, a network
// hiccup — and is worth retrying with backoff before giving up.
//
// Classification happens once, at the HTTP-call layer
// (internal/core/mcp/servers' doAndDecode and its two hand-rolled
// exceptions), never downstream of a real MCP client/server round trip.
// This is a real constraint, not a style choice: per
// mcp.ToolHandlerFor's own doc comment, "an error result is treated as a
// tool error, rather than a protocol error, and is therefore packed into
// CallToolResult.Content, with IsError set" — a tool handler's returned
// Go error becomes plain text once it crosses the protocol boundary, so
// a wrapped sentinel on it does NOT survive to Gateway.ExecuteTool's
// caller. Retry decisions have to be made before a tool handler returns,
// not after.
var (
	ErrToolTerminal  = errors.New("mcp: tool call failed (not retryable)")
	ErrToolRetryable = errors.New("mcp: tool call failed (retryable)")
)

// ClassifyToolHTTPError maps a third-party API's HTTP status code to
// ErrToolTerminal or ErrToolRetryable. 429 (rate limited) and 5xx
// (server error) are retryable; every other non-2xx status — 400/401/
// 403/404/409/422, etc. — is terminal, since retrying an outright
// rejection won't change the outcome.
func ClassifyToolHTTPError(statusCode int, detail string) error {
	if statusCode == 429 || statusCode >= 500 {
		return fmt.Errorf("%w: status %d: %s", ErrToolRetryable, statusCode, detail)
	}
	return fmt.Errorf("%w: status %d: %s", ErrToolTerminal, statusCode, detail)
}
