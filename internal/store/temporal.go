package store

import (
	"fmt"
	"strings"
	"time"
)

// temporalRelevanceBoost is Phase E's real fix
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md) for a real, confirmed gap:
// retrieval ranked purely by embedding/keyword similarity to the
// query's words has no way to prefer a summary whose own period
// actually matches a timeframe the question implies. Real-verified via
// a deliberately adversarial synthetic test: two summaries about the
// same kind of thing, 4 months apart, one worded closely enough to the
// query to fused-outrank the other despite being the wrong period for a
// question asking about "last month." Set to a full reciprocal-rank-0
// contribution (comparable to being the single best vector or keyword
// match) — strong enough to flip that real case, without being an
// unconditional override; a candidate with a much stronger textual
// match can still win some other way. Not yet measured against a wider
// sweep the way this file's other thresholds were — a reasoned
// starting point, flagged for calibration once verified against more
// real cases.
const temporalRelevanceBoost = 1.0

// timeframeKeywords backs resolveQueryTimeframe — a small, deliberately
// narrow set of common relative-time phrases, same cheap substring-match
// posture as recommendationKeywords/orderingKeywords. Checked longest-
// phrase-first within each pair (e.g. "last week" before a hypothetical
// bare "week") isn't needed here since every phrase already includes
// "this"/"last"/"yesterday"/"today", so there's no shorter phrase that's
// also a real match for a different timeframe.
type timeframeResolver func(today time.Time) (start, end time.Time)

var timeframeKeywords = []struct {
	phrase   string
	resolver timeframeResolver
}{
	{"yesterday", func(today time.Time) (time.Time, time.Time) {
		y := today.AddDate(0, 0, -1)
		return y, y.AddDate(0, 0, 1)
	}},
	{"today", func(today time.Time) (time.Time, time.Time) {
		return today, today.AddDate(0, 0, 1)
	}},
	{"last week", func(today time.Time) (time.Time, time.Time) {
		monday := mondayOfWeek(today).AddDate(0, 0, -7)
		return monday, monday.AddDate(0, 0, 7)
	}},
	{"this week", func(today time.Time) (time.Time, time.Time) {
		monday := mondayOfWeek(today)
		return monday, monday.AddDate(0, 0, 7)
	}},
	{"last month", func(today time.Time) (time.Time, time.Time) {
		thisMonthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
		return thisMonthStart.AddDate(0, -1, 0), thisMonthStart
	}},
	{"this month", func(today time.Time) (time.Time, time.Time) {
		thisMonthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
		return thisMonthStart, thisMonthStart.AddDate(0, 1, 0)
	}},
	{"last year", func(today time.Time) (time.Time, time.Time) {
		thisYearStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, today.Location())
		return thisYearStart.AddDate(-1, 0, 0), thisYearStart
	}},
	{"this year", func(today time.Time) (time.Time, time.Time) {
		thisYearStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, today.Location())
		return thisYearStart, thisYearStart.AddDate(1, 0, 0)
	}},
}

// resolveQueryTimeframe detects a small, deliberately narrow set of
// common relative-time phrases in query and resolves them to a concrete
// [start, end) range relative to now — enough to cover the real,
// confirmed failure shape ("last month") and its natural siblings, not
// a general date-phrase parser. Phrases like "in January" or "since my
// trip" are deliberately out of scope: resolving them correctly needs
// more context (which January? relative to what?) than a cheap
// substring match can respect, and a wrong guess here actively hurts
// ranking rather than just doing nothing.
func resolveQueryTimeframe(query string, now time.Time) (start, end time.Time, ok bool) {
	lower := strings.ToLower(query)
	today := now.Truncate(24 * time.Hour)
	for _, tk := range timeframeKeywords {
		if strings.Contains(lower, tk.phrase) {
			s, e := tk.resolver(today)
			return s, e, true
		}
	}
	return time.Time{}, time.Time{}, false
}

// mondayOfWeek returns the Monday (00:00) of day's own ISO week.
func mondayOfWeek(day time.Time) time.Time {
	wd := int(day.Weekday())
	if wd == 0 { // Go's Weekday: Sunday=0 — ISO treats Sunday as day 7
		wd = 7
	}
	return day.AddDate(0, 0, -(wd - 1))
}

// isoWeekStart returns the Monday (00:00 UTC) of ISO week `week` in ISO
// year `year` — the standard "Jan 4th is always in week 1" algorithm,
// self-verified in tests via time.Time.ISOWeek's own reverse mapping
// rather than a hand-checked calendar.
func isoWeekStart(year, week int) time.Time {
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	return mondayOfWeek(jan4).AddDate(0, 0, (week-1)*7)
}

// parsePeriodRange parses one of summaries.period's own formats
// ("YYYY-MM-DD" daily, "YYYY-Www" weekly, "YYYY-MM" monthly, "YYYY"
// yearly — schema/0001_init.sql) into the concrete [start, end) range it
// represents, or ok=false for a period string in none of those shapes.
func parsePeriodRange(period string) (start, end time.Time, ok bool) {
	if t, err := time.Parse("2006-01-02", period); err == nil {
		return t, t.AddDate(0, 0, 1), true
	}
	if t, err := time.Parse("2006-01", period); err == nil {
		return t, t.AddDate(0, 1, 0), true
	}
	if len(period) == 8 && period[4] == '-' && period[5] == 'W' {
		var year, week int
		if n, err := fmt.Sscanf(period, "%d-W%d", &year, &week); err == nil && n == 2 {
			s := isoWeekStart(year, week)
			return s, s.AddDate(0, 0, 7), true
		}
	}
	if t, err := time.Parse("2006", period); err == nil {
		return t, t.AddDate(1, 0, 0), true
	}
	return time.Time{}, time.Time{}, false
}

// periodsOverlap reports whether [aStart, aEnd) and [bStart, bEnd)
// share any real time — the standard half-open-interval overlap check.
func periodsOverlap(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}
