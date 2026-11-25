package daemon

import (
	"testing"
	"time"
)

// Standard cron: when both day-of-month and day-of-week are restricted, either one matching fires.
func TestCronDayOfMonthAndDayOfWeekAreORed(t *testing.T) {
	s, err := ParseSchedule("0 9 1 * 1") // 09:00 on the 1st, and on every Monday
	if err != nil {
		t.Fatal(err)
	}
	// Wednesday 2026-07-01 is the 1st but not a Monday.
	from := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) // Tuesday
	got := s.Next(from)
	want := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s (the 1st)", got, want)
	}
	// Next after that is Monday 2026-07-06.
	got = s.Next(want)
	want = time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s (Monday)", got, want)
	}

	// With a wildcard day-of-month only the weekday restricts.
	weekdays, err := ParseSchedule("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	sat := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC) // Saturday
	if got := weekdays.Next(sat); got.Weekday() != time.Monday {
		t.Fatalf("weekday schedule fired on %s", got.Weekday())
	}
}

func TestInvalidScheduleIsRejectedNotDefaulted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Triggers = []TriggerSpec{{ID: "daily", Type: "cron", Interval: "@daily"}}
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("expected ValidateConfig to reject an unparseable cron schedule")
	}
	if _, err := pollerSchedule(TriggerSpec{ID: "s", Type: "sentinel"}); err != nil {
		t.Fatalf("sentinel without interval should default to 1m: %v", err)
	}
}

func TestCoalescerKeepsDistinctIdempotencyKeysApart(t *testing.T) {
	spec := TriggerSpec{ID: "gh", Type: "webhook", Debounce: "30s", IdempotencyKey: "{{.pr}}"}
	a := Event{TriggerID: "gh", Fields: map[string]any{"pr": "41"}, ReceivedAt: time.Now()}
	b := Event{TriggerID: "gh", Fields: map[string]any{"pr": "42"}, ReceivedAt: time.Now()}
	if bucketKey(spec, a) == bucketKey(spec, b) {
		t.Fatal("events with different idempotency keys must not share a debounce bucket")
	}
}
