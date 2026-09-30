package consolidation

import (
	"fmt"
	"time"
)

// periodAsOfDate resolves a summary's level+period into the single,
// real-world calendar date it represents — the entities.first_seen/
// last_updated columns this backs (schema/0001_init.sql) are a single
// `date`, not a range, so a period spanning more than one day (weekly,
// monthly, yearly) needs one representative date. Uses the *last* day
// within the period, not the first — "as of" a period most naturally
// means "as of its most recent day."
//
// Real, confirmed fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): upsertEntities
// previously stamped every touched entity with Postgres's own current_date
// — the real wall-clock day the consolidation job happened to run, not the
// (possibly simulated/historical, for backdated benchmark or import
// replay) date actually being consolidated. A summary's own period
// column and an episode's own ts already carry the correct date; entities
// had no equivalent until this.
//
// Independently implemented from internal/store/temporal.go's own
// parsePeriodRange rather than shared across packages — both parsers are
// small and stable; the cross-package coupling importing one from the
// other would need outweighs deduplicating roughly a dozen lines.
func periodAsOfDate(level, period string) (time.Time, bool) {
	switch level {
	case "daily":
		t, err := time.Parse("2006-01-02", period)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	case "weekly":
		var year, week int
		if n, err := fmt.Sscanf(period, "%d-W%d", &year, &week); err != nil || n != 2 {
			return time.Time{}, false
		}
		return isoWeekStart(year, week).AddDate(0, 0, 6), true // Sunday, the week's last day
	case "monthly":
		t, err := time.Parse("2006-01", period)
		if err != nil {
			return time.Time{}, false
		}
		return t.AddDate(0, 1, -1), true // last day of that month
	case "yearly":
		t, err := time.Parse("2006", period)
		if err != nil {
			return time.Time{}, false
		}
		return t.AddDate(1, 0, -1), true // December 31 of that year
	default:
		return time.Time{}, false
	}
}

// isoWeekStart returns the Monday (00:00 UTC) of ISO week `week` in ISO
// year `year` — the standard "Jan 4th is always in week 1" algorithm,
// mirroring internal/store/temporal.go's own copy (see periodAsOfDate's
// doc comment for why this isn't shared across packages).
func isoWeekStart(year, week int) time.Time {
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	wd := int(jan4.Weekday())
	if wd == 0 { // Go's Weekday: Sunday=0 — ISO treats Sunday as day 7
		wd = 7
	}
	monday := jan4.AddDate(0, 0, -(wd - 1))
	return monday.AddDate(0, 0, (week-1)*7)
}
