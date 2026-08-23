package workflows

import (
	"testing"
	"time"
)

func TestComputeSchedule_NonScheduledTriggerTypesIgnoreCron(t *testing.T) {
	for _, triggerType := range []string{"manual", "webhook"} {
		cron, nextRunAt, code, msg := computeSchedule(triggerType, nil)
		if cron != nil || nextRunAt.Valid || code != "" || msg != "" {
			t.Errorf("computeSchedule(%q, nil) = (%v, %v, %q, %q), want all zero values", triggerType, cron, nextRunAt, code, msg)
		}
	}
}

func TestComputeSchedule_ScheduledRequiresCron(t *testing.T) {
	_, _, code, _ := computeSchedule("scheduled", nil)
	if code != "CRON_EXPRESSION_REQUIRED" {
		t.Errorf("code = %q, want CRON_EXPRESSION_REQUIRED", code)
	}

	empty := ""
	_, _, code, _ = computeSchedule("scheduled", &empty)
	if code != "CRON_EXPRESSION_REQUIRED" {
		t.Errorf("code (empty string) = %q, want CRON_EXPRESSION_REQUIRED", code)
	}
}

func TestComputeSchedule_RejectsInvalidCron(t *testing.T) {
	bad := "not a cron expression"
	_, _, code, msg := computeSchedule("scheduled", &bad)
	if code != "INVALID_CRON_EXPRESSION" {
		t.Errorf("code = %q, want INVALID_CRON_EXPRESSION", code)
	}
	if msg == "" {
		t.Error("msg is empty, want the underlying parse error included")
	}
}

func TestComputeSchedule_ValidCronComputesFutureNextRunAt(t *testing.T) {
	expr := "0 9 * * 1" // every Monday at 9am
	cron, nextRunAt, code, msg := computeSchedule("scheduled", &expr)
	if code != "" || msg != "" {
		t.Fatalf("computeSchedule returned an error: %s / %s", code, msg)
	}
	if cron == nil || *cron != expr {
		t.Fatalf("cron = %v, want %q", cron, expr)
	}
	if !nextRunAt.Valid {
		t.Fatal("next_run_at is not valid")
	}
	if !nextRunAt.Time.After(time.Now()) {
		t.Fatalf("next_run_at = %v, want a time after now", nextRunAt.Time)
	}
	if nextRunAt.Time.Weekday() != time.Monday {
		t.Fatalf("next_run_at weekday = %v, want Monday", nextRunAt.Time.Weekday())
	}
}
