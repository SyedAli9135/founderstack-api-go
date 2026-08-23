package mcp

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// tokenMetaKey is the CallToolParams.Meta key Gateway.ExecuteTool stamps
// the org's decrypted integration token under.
//
// Not a context.Context value: InMemoryTransport (what internal/core/mcp
// wires every tool server up with) is real newline-delimited JSON
// framing, not a same-process object handoff — "in-memory" only means no
// subprocess/socket, not "no serialization." A context.Value set on the
// client's ctx before calling CallTool never reaches the server-side
// handler, because the server's ctx comes from its own session's
// message-read loop, not from the client's call. _meta is the actual
// MCP-spec-sanctioned side channel for exactly this: transmitted
// alongside a tool call, but never part of Arguments — the only piece of
// the request an LLM's tool-calling schema/response ever includes.
const tokenMetaKey = "founderstack.integration_token"

// WithToken returns CallToolParams.Meta carrying token — Gateway.ExecuteTool
// is the one real caller in production; exported so
// internal/core/mcp/servers' tests can call tools through a real MCP
// session (AddTool's schema validation included) without going through a
// full Gateway + database.
func WithToken(token string) gomcp.Meta {
	return gomcp.Meta{tokenMetaKey: token}
}

// TokenFromRequest returns the decrypted integration token
// Gateway.ExecuteTool attached to this call's _meta. Every tool handler
// in internal/core/mcp/servers reads its credential this way instead of
// fetching it itself — keeping the GetIntegrationToken decrypt call to
// exactly one call site (Gateway.ExecuteTool), not one per tool handler.
func TokenFromRequest(req *gomcp.CallToolRequest) (string, bool) {
	if req == nil || req.Params == nil {
		return "", false
	}
	tok, ok := req.Params.Meta[tokenMetaKey].(string)
	return tok, ok
}

// extraMetaKey carries a connection's Token.Extra map (workflow 4's
// provider-specific fields — Discord's incoming-webhook id/token/url,
// e.g.) — additive to tokenMetaKey, not a replacement: the 5 original
// tool servers only ever need the bare token, so WithToken/TokenFromRequest
// keep their existing signatures unchanged.
const extraMetaKey = "founderstack.integration_extra"

// WithExtra returns CallToolParams.Meta carrying extra, for merging with
// WithToken's map (Gateway.ExecuteTool does this whenever a connection's
// Token.Extra is non-empty — most connections have none).
func WithExtra(extra map[string]string) gomcp.Meta {
	return gomcp.Meta{extraMetaKey: extra}
}

// ExtraFromRequest returns the Token.Extra map Gateway.ExecuteTool
// attached to this call's _meta, if any. Unlike TokenFromRequest, the
// value survives a real MCP round trip (InMemoryTransport's actual JSON
// framing — see tokenMetaKey's doc comment) as map[string]any, not
// map[string]string, since Go's JSON decoder has no static type to
// target; this converts it back.
func ExtraFromRequest(req *gomcp.CallToolRequest) (map[string]string, bool) {
	if req == nil || req.Params == nil {
		return nil, false
	}
	raw, ok := req.Params.Meta[extraMetaKey]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case map[string]string:
		return v, true
	case map[string]any:
		extra := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				extra[k] = s
			}
		}
		return extra, true
	default:
		return nil, false
	}
}
