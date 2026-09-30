package store

import (
	"testing"
	"time"
)

// TestResolveQueryTimeframeLastMonth is the real, confirmed case
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase E) — an adversarial
// synthetic test found retrieval picking a temporally-wrong but
// lexically-closer summary for exactly this phrasing.
func TestResolveQueryTimeframeLastMonth(t *testing.T) {
	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC)
	start, end, ok := resolveQueryTimeframe("What did I learn about at the AI conference I attended last month?", now)
	if !ok {
		t.Fatal("resolveQueryTimeframe() = ok=false, want a resolved 'last month' range")
	}
	wantStart := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Errorf("resolveQueryTimeframe() = [%v, %v), want [%v, %v)", start, end, wantStart, wantEnd)
	}
}

func TestResolveQueryTimeframeAllPhrases(t *testing.T) {
	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // a Wednesday
	cases := []struct {
		query     string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{"what happened today?", time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 6, 0, 0, 0, 0, time.UTC)},
		{"what did I do yesterday?", time.Date(2024, 6, 4, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC)},
		{"what happened this week?", time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
		{"what happened last week?", time.Date(2024, 5, 27, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC)},
		{"what happened this month?", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"what happened this year?", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"what happened last year?", time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		start, end, ok := resolveQueryTimeframe(tc.query, now)
		if !ok {
			t.Errorf("resolveQueryTimeframe(%q) = ok=false, want a resolved range", tc.query)
			continue
		}
		if !start.Equal(tc.wantStart) || !end.Equal(tc.wantEnd) {
			t.Errorf("resolveQueryTimeframe(%q) = [%v, %v), want [%v, %v)", tc.query, start, end, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestResolveQueryTimeframeNoPhraseReturnsNotOK(t *testing.T) {
	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC)
	if _, _, ok := resolveQueryTimeframe("what database does Meridian use?", now); ok {
		t.Error("resolveQueryTimeframe() = ok=true for a query with no relative-time phrase at all")
	}
}

// TestParsePeriodRangeAllFormats covers every real summaries.period
// shape (schema/0001_init.sql): daily, weekly, monthly, yearly.
func TestParsePeriodRangeAllFormats(t *testing.T) {
	cases := []struct {
		period    string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{"2023-05-15", time.Date(2023, 5, 15, 0, 0, 0, 0, time.UTC), time.Date(2023, 5, 16, 0, 0, 0, 0, time.UTC)},
		{"2023-05", time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)},
		{"2023", time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		start, end, ok := parsePeriodRange(tc.period)
		if !ok {
			t.Errorf("parsePeriodRange(%q) = ok=false, want a resolved range", tc.period)
			continue
		}
		if !start.Equal(tc.wantStart) || !end.Equal(tc.wantEnd) {
			t.Errorf("parsePeriodRange(%q) = [%v, %v), want [%v, %v)", tc.period, start, end, tc.wantStart, tc.wantEnd)
		}
	}
}

// TestParsePeriodRangeWeeklySelfConsistentWithISOWeek confirms
// isoWeekStart's own correctness via time.Time.ISOWeek's reverse
// mapping — a real, mechanical check rather than a hand-verified
// calendar date, since ISO week arithmetic is exactly the kind of thing
// easy to get subtly wrong at year boundaries.
func TestParsePeriodRangeWeeklySelfConsistentWithISOWeek(t *testing.T) {
	for _, period := range []string{"2023-W01", "2023-W26", "2023-W52", "2020-W53", "2024-W01"} {
		start, end, ok := parsePeriodRange(period)
		if !ok {
			t.Errorf("parsePeriodRange(%q) = ok=false, want a resolved range", period)
			continue
		}
		if start.Weekday() != time.Monday {
			t.Errorf("parsePeriodRange(%q) start = %v, want a Monday", period, start)
		}
		if got := end.Sub(start); got != 7*24*time.Hour {
			t.Errorf("parsePeriodRange(%q) span = %v, want exactly 7 days", period, got)
		}
		gotYear, gotWeek := start.ISOWeek()
		var wantYear, wantWeek int
		splitISOWeek(period, &wantYear, &wantWeek)
		if gotYear != wantYear || gotWeek != wantWeek {
			t.Errorf("parsePeriodRange(%q) start's own ISOWeek() = %d-W%02d, want %d-W%02d", period, gotYear, gotWeek, wantYear, wantWeek)
		}
	}
}

func splitISOWeek(period string, year, week *int) {
	var y, w int
	for _, c := range []byte(period[:4]) {
		y = y*10 + int(c-'0')
	}
	for _, c := range []byte(period[6:]) {
		w = w*10 + int(c-'0')
	}
	*year, *week = y, w
}

func TestParsePeriodRangeUnrecognizedFormat(t *testing.T) {
	if _, _, ok := parsePeriodRange("not-a-period"); ok {
		t.Error("parsePeriodRange() = ok=true for a nonsense string")
	}
}

func TestPeriodsOverlap(t *testing.T) {
	day := func(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name                       string
		aStart, aEnd, bStart, bEnd time.Time
		want                       bool
	}{
		{"identical ranges", day(2024, 5, 1), day(2024, 6, 1), day(2024, 5, 1), day(2024, 6, 1), true},
		{"a fully inside b", day(2024, 5, 10), day(2024, 5, 11), day(2024, 5, 1), day(2024, 6, 1), true},
		{"adjacent, not overlapping (half-open)", day(2024, 5, 1), day(2024, 6, 1), day(2024, 6, 1), day(2024, 7, 1), false},
		{"disjoint", day(2024, 1, 1), day(2024, 2, 1), day(2024, 5, 1), day(2024, 6, 1), false},
		{"partial overlap", day(2024, 5, 15), day(2024, 6, 15), day(2024, 5, 1), day(2024, 6, 1), true},
	}
	for _, tc := range cases {
		if got := periodsOverlap(tc.aStart, tc.aEnd, tc.bStart, tc.bEnd); got != tc.want {
			t.Errorf("%s: periodsOverlap() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
