// Package cronutil validates the cron expressions workflows are scheduled with.
package cronutil

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// MinInterval is the shortest gap allowed between two runs of a scheduled
// workflow. Every run spends the customer's own LLM key (and can notify the
// team), so "every minute" is a billing and spam hazard, not a feature.
const MinInterval = 5 * time.Minute

// Validate parses a standard 5-field expression and refuses one that can fire
// more often than MinInterval anywhere in the next ~100 hours of firings
// (enough to see a burst such as "*/1 9 * * *" that is quiet most of the day).
func Validate(expr string) (cron.Schedule, error) {
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, err
	}
	if gap := shortestGap(s); gap > 0 && gap < MinInterval {
		return nil, fmt.Errorf("it would run every %s; the minimum interval between runs is %s", gap.Round(time.Second), MinInterval)
	}
	return s, nil
}

// shortestGap is the smallest gap among the next 200 firings (0 if fewer than two).
func shortestGap(s cron.Schedule) time.Duration {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var prev time.Time
	var min time.Duration
	for i := 0; i < 200; i++ {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		if !prev.IsZero() {
			if d := t.Sub(prev); min == 0 || d < min {
				min = d
			}
		}
		prev = t
	}
	return min
}
