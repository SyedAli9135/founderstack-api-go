package mcp

import (
	"errors"
	"testing"
)

func TestClassifyToolHTTPError(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{400, ErrToolTerminal},
		{401, ErrToolTerminal},
		{403, ErrToolTerminal},
		{404, ErrToolTerminal},
		{409, ErrToolTerminal},
		{422, ErrToolTerminal},
		{429, ErrToolRetryable},
		{500, ErrToolRetryable},
		{502, ErrToolRetryable},
		{503, ErrToolRetryable},
	}
	for _, c := range cases {
		err := ClassifyToolHTTPError(c.status, "boom")
		if !errors.Is(err, c.want) {
			t.Errorf("status %d: err = %v, want wrapping %v", c.status, err, c.want)
		}
		other := ErrToolTerminal
		if c.want == ErrToolTerminal {
			other = ErrToolRetryable
		}
		if errors.Is(err, other) {
			t.Errorf("status %d: err = %v, should not also match %v", c.status, err, other)
		}
	}
}
