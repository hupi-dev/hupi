package store

import (
	"fmt"
	"math"
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

// relativeDateLabel turns a start date into a human-readable "N weeks
// before now"-style phrase, or "" if start is somehow in the future.
// Real, deliberate fix for answer-time reasoning failures
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): three separate real cases
// this document tracked all had the correct facts and dates present in
// context, with the model still getting the arithmetic wrong (e.g.
// answering "9 weeks ago" against a gold "3 weeks ago"). Computing the
// delta here, in code, and handing the model the already-correct answer
// removes that arithmetic from its plate entirely — the same "shift work
// out of the LLM and into deterministic code" approach that already
// worked for resolveQueryTimeframe/parsePeriodRange themselves.
//
// Takes an already-resolved time.Time, not a period string — callers
// with a summary's period parse it via parsePeriodRange first; callers
// with an exact timestamp (an episode's ts, an entity's last_updated)
// pass it directly, no format-then-reparse round trip needed.
//
// Rounds to the nearest unit, not floor — 20 days rounds to "3 weeks",
// matching how LongMemEval's own gold answers phrase this (20/7 = 2.86,
// a human says "about 3 weeks," not "2 weeks" truncated). Tiers roughly
// match everyday phrasing: days below a week, weeks below ~2 months,
// months below ~2 years, years beyond that — a summary's own period
// granularity (daily/weekly/monthly/yearly) already bounds how precise
// the *input* date is, so there's no value in offering sub-day precision
// output for something derived from a monthly summary's period start.
func relativeDateLabel(start, now time.Time) string {
	days := int(now.Sub(start).Hours() / 24)
	switch {
	case days < 0:
		return ""
	case days == 0:
		return "today"
	case days == 1:
		return "1 day before now"
	case days < 7:
		return fmt.Sprintf("%d days before now", days)
	case days < 60:
		return pluralUnit(math.Round(float64(days)/7), "week")
	default:
		// Decided by the rounded unit count, not a fixed day threshold —
		// a fixed cutover (e.g. "months below 730 days") would render a
		// 366-day gap as "12 months before now" instead of the more
		// natural "1 year before now" right at the boundary where it
		// matters most.
		if months := math.Round(float64(days) / 30.44); months < 12 {
			return pluralUnit(months, "month")
		}
		return pluralUnit(math.Round(float64(days)/365.25), "year")
	}
}

// pluralUnit renders a rounded count with its unit, singular at exactly
// one — n is never <= 0 in practice (every tier above only reaches this
// with at least one full unit elapsed), but clamps to 1 defensively
// rather than emitting a nonsensical "0 weeks before now".
func pluralUnit(n float64, unit string) string {
	count := int(n)
	if count <= 0 {
		count = 1
	}
	if count == 1 {
		return fmt.Sprintf("1 %s before now", unit)
	}
	return fmt.Sprintf("%d %ss before now", count, unit)
}
