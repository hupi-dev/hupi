package consolidation

import (
	"fmt"
	"testing"
	"time"
)

func TestPeriodAsOfDate(t *testing.T) {
	day := func(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		level, period string
		want          time.Time
	}{
		{"daily", "2023-04-16", day(2023, 4, 16)},
		{"weekly", "2023-W13", day(2023, 4, 2)},   // 2023-W13 is Mar 27 - Apr 2; last day is Apr 2
		{"monthly", "2023-04", day(2023, 4, 30)},  // last day of April
		{"monthly", "2024-02", day(2024, 2, 29)},  // leap year February
		{"yearly", "2023", day(2023, 12, 31)},
	}
	for _, tc := range cases {
		got, ok := periodAsOfDate(tc.level, tc.period)
		if !ok {
			t.Errorf("periodAsOfDate(%q, %q) = ok=false, want a resolved date", tc.level, tc.period)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("periodAsOfDate(%q, %q) = %v, want %v", tc.level, tc.period, got, tc.want)
		}
	}
}

func TestPeriodAsOfDateUnrecognizedInputs(t *testing.T) {
	cases := []struct{ level, period string }{
		{"daily", "not-a-date"},
		{"weekly", "2023-13"},
		{"monthly", "2023"},
		{"yearly", "2023-01"},
		{"quarterly", "2023-Q1"},
	}
	for _, tc := range cases {
		if _, ok := periodAsOfDate(tc.level, tc.period); ok {
			t.Errorf("periodAsOfDate(%q, %q) = ok=true, want false", tc.level, tc.period)
		}
	}
}

// TestPeriodAsOfDateWeeklySelfConsistentWithISOWeek mirrors
// internal/store/temporal.go's own equivalent test — real ISO week
// arithmetic is easy to get subtly wrong at year boundaries, so this
// checks isoWeekStart's result against time.Time.ISOWeek's own reverse
// mapping rather than a hand-verified calendar date.
func TestPeriodAsOfDateWeeklySelfConsistentWithISOWeek(t *testing.T) {
	for _, tc := range []struct {
		year, week int
	}{
		{2023, 1}, {2023, 26}, {2023, 52}, {2020, 53}, {2024, 1},
	} {
		period := fmt.Sprintf("%d-W%02d", tc.year, tc.week)
		lastDay, ok := periodAsOfDate("weekly", period)
		if !ok {
			t.Errorf("periodAsOfDate(weekly, %q) = ok=false", period)
			continue
		}
		monday := lastDay.AddDate(0, 0, -6)
		if monday.Weekday() != time.Monday {
			t.Errorf("isoWeekStart(%d, %d) = %v, want a Monday", tc.year, tc.week, monday)
		}
		gotYear, gotWeek := monday.ISOWeek()
		if gotYear != tc.year || gotWeek != tc.week {
			t.Errorf("isoWeekStart(%d, %d)'s own ISOWeek() = %d-W%02d, want %d-W%02d", tc.year, tc.week, gotYear, gotWeek, tc.year, tc.week)
		}
	}
}
