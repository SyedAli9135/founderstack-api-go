package mcp

import (
	"errors"
	"fmt"
)

var (
	ErrToolTerminal  = errors.New("mcp: tool call failed (not retryable)")
	ErrToolRetryable = errors.New("mcp: tool call failed (retryable)")
)

func ClassifyToolHTTPError(statusCode int, detail string) error {
	if statusCode == 429 || statusCode >= 500 {
		return fmt.Errorf("%w: status %d: %s", ErrToolRetryable, statusCode, detail)
	}
	return fmt.Errorf("%w: status %d: %s", ErrToolTerminal, statusCode, detail)
}
