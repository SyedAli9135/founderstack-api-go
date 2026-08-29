package mcp

import gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

// tokenMetaKey is the CallToolParams.Meta key Gateway.ExecuteTool stamps
// the org's decrypted integration token under.
const tokenMetaKey = "founderstack.integration_token"

// WithToken returns CallToolParams.Meta carrying token — Gateway.ExecuteTool
// is the one real caller in production
func WithToken(token string) gomcp.Meta {
	return gomcp.Meta{tokenMetaKey: token}
}

// TokenFromRequest returns the decrypted integration token
// Gateway.ExecuteTool attached to this call's _meta.
func TokenFromRequest(req *gomcp.CallToolRequest) (string, bool) {
	if req == nil || req.Params == nil {
		return "", false
	}
	tok, ok := req.Params.Meta[tokenMetaKey].(string)
	return tok, ok
}

// extraMetaKey carries a connection's Token.Extra map — additive to tokenMetaKey, not a replacement: the 5 original
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
// attached to this call's _meta.
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

// idempotencyKeyMetaKey carries a deterministic per-tool-call key
// (`{run_id}-{tool_call_index}`
const idempotencyKeyMetaKey = "founderstack.idempotency_key"

// WithIdempotencyKey returns CallToolParams.Meta carrying key.
func WithIdempotencyKey(key string) gomcp.Meta {
	return gomcp.Meta{idempotencyKeyMetaKey: key}
}

// IdempotencyKeyFromRequest returns the idempotency key
// Gateway.ExecuteTool attached to this call's _meta, if any.
func IdempotencyKeyFromRequest(req *gomcp.CallToolRequest) (string, bool) {
	if req == nil || req.Params == nil {
		return "", false
	}
	key, ok := req.Params.Meta[idempotencyKeyMetaKey].(string)
	return key, ok
}
