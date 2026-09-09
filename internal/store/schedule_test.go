package store

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	cases := []struct {
		name string
		expr string
		ok   bool
	}{
		{"daily at 2am", "0 2 * * *", true},
		{"every fifteen minutes", "*/15 * * * *", true},
		{"weekdays", "30 6 * * 1-5", true},
		{"descriptor", "@daily", true},
		{"interval descriptor", "@every 6h", true},
		{"surrounding whitespace is tolerated", "  0 2 * * *  ", true},

		{"empty", "", false},
		{"only whitespace", "   ", false},
		{"too few fields", "* * *", false},
		{"minute out of range", "99 2 * * *", false},
		{"hour out of range", "0 25 * * *", false},
		{"not cron at all", "every night please", false},
		// Six fields is the seconds form, which this deliberately refuses:
		// the tick is coarser than a second, so accepting it would mean
		// running at a cadence the expression does not describe.
		{"seconds form", "0 0 2 * * *", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched, err := ParseSchedule(tc.expr)
			if tc.ok {
				if err != nil {
					t.Fatalf("ParseSchedule(%q) = %v, want success", tc.expr, err)
				}
				if sched == nil {
					t.Fatal("a valid expression returned a nil schedule")
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseSchedule(%q) succeeded, want an error", tc.expr)
			}
			// The message a user sees has to name what they typed and how the
			// fields are ordered; a bare library error does neither.
			if tc.expr != "" && len(tc.expr) > 3 && !contains(err.Error(), "minute hour day-of-month") {
				t.Fatalf("the error does not explain the field order: %v", err)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestNextRun(t *testing.T) {
	base := time.Date(2026, 9, 5, 1, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		expr string
		want time.Time
	}{
		{"later the same day", "0 2 * * *", time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)},
		{"already past today, so tomorrow", "0 1 * * *", time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)},
		{"next quarter hour", "*/15 * * * *", time.Date(2026, 9, 5, 1, 45, 0, 0, time.UTC)},
		// A valid expression for a date that never occurs. It must parse and
		// return the zero time rather than erroring, because callers treat a
		// zero as "unscheduled" and an error as "broken configuration".
		{"the 30th of February", "0 0 30 2 *", time.Time{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NextRun(tc.expr, base)
			if err != nil {
				t.Fatalf("NextRun(%q) = %v", tc.expr, err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("NextRun(%q, %v) = %v, want %v", tc.expr, base, got, tc.want)
			}
		})
	}

	// Strictly after, never equal: a schedule that returned `now` would fire
	// again on the same tick, and then again on the next one.
	exact := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)
	next, err := NextRun("0 2 * * *", exact)
	if err != nil {
		t.Fatal(err)
	}
	if !next.After(exact) {
		t.Fatalf("NextRun at the exact firing time returned %v, which is not after %v", next, exact)
	}
}

func TestRefreshNextRun(t *testing.T) {
	now := time.Date(2026, 9, 5, 1, 30, 0, 0, time.UTC)

	t.Run("a scheduled, enabled job gets a firing time", func(t *testing.T) {
		j := &Job{ScheduleCron: "0 2 * * *", Enabled: true}
		j.RefreshNextRun(now)
		if want := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC); !j.NextRunAt.Equal(want) {
			t.Fatalf("NextRunAt = %v, want %v", j.NextRunAt, want)
		}
	})

	// Both of these clear the field rather than leaving a stale value. A
	// disabled job showing "next run: 2am" on the dashboard would be a lie,
	// and a lie in the direction of "your backups are fine".
	t.Run("disabling clears the firing time", func(t *testing.T) {
		j := &Job{ScheduleCron: "0 2 * * *", Enabled: false,
			NextRunAt: time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)}
		j.RefreshNextRun(now)
		if !j.NextRunAt.IsZero() {
			t.Fatalf("a disabled job kept NextRunAt = %v", j.NextRunAt)
		}
	})

	t.Run("clearing the schedule clears the firing time", func(t *testing.T) {
		j := &Job{ScheduleCron: "", Enabled: true,
			NextRunAt: time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)}
		j.RefreshNextRun(now)
		if !j.NextRunAt.IsZero() {
			t.Fatalf("an unscheduled job kept NextRunAt = %v", j.NextRunAt)
		}
	})
}

// A zero next-run must round-trip as empty, not as year 0001 — which would
// read as "due since the first century" to any query without the
// schedule_cron guard.
func TestFormatNextRunStoresZeroAsEmpty(t *testing.T) {
	if got := formatNextRun(time.Time{}); got != "" {
		t.Fatalf("formatNextRun(zero) = %q, want an empty string", got)
	}
	at := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)
	if got := parseTime(formatNextRun(at)); !got.Equal(at) {
		t.Fatalf("a real time did not round-trip: %v -> %v", at, got)
	}
}
