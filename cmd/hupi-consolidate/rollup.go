package main

import (
	"fmt"
	"time"
)

// rollupJob is exactly the argument shape Runner.RunRollup takes — see
// docs/GAP_CLOSURE_PLAN.md §4.1 for why each boundary below is the day it
// is. Computing this is calendar logic that belongs here, in the cron
// scheduling layer, not in internal/consolidation (runner.go:249's doc
// comment already drew that line before this file existed).
type rollupJob struct {
	level         string
	sourceLevel   string
	period        string
	sourcePeriods []string
}

// dueRollups returns every rollup that's ready given date is the day
// whose daily summary this run just produced (or confirmed already
// exists) for every active scope. Independent checks — a date can be
// both a month-end and a year-end boundary (Jan 1 is always the 1st too).
func dueRollups(date time.Time) []rollupJob {
	var jobs []rollupJob
	if j, ok := weeklyRollup(date); ok {
		jobs = append(jobs, j)
	}
	if j, ok := monthlyRollup(date); ok {
		jobs = append(jobs, j)
	}
	if j, ok := yearlyRollup(date); ok {
		jobs = append(jobs, j)
	}
	return jobs
}

// weeklyRollup fires when date is a Monday: every day of the ISO week
// ending the Sunday just before it already has a daily summary, from
// this same command's prior runs.
func weeklyRollup(date time.Time) (rollupJob, bool) {
	if date.Weekday() != time.Monday {
		return rollupJob{}, false
	}
	lastDay := date.AddDate(0, 0, -1)
	year, week := lastDay.ISOWeek()

	sourcePeriods := make([]string, 0, 7)
	for d := date.AddDate(0, 0, -7); !d.After(lastDay); d = d.AddDate(0, 0, 1) {
		sourcePeriods = append(sourcePeriods, d.Format("2006-01-02"))
	}
	return rollupJob{
		level: "weekly", sourceLevel: "daily",
		period:        fmt.Sprintf("%04d-W%02d", year, week),
		sourcePeriods: sourcePeriods,
	}, true
}

// monthlyRollup fires on the 1st, rolling up the month that just ended.
// A week is attributed to the month containing its Monday (start), even
// when that week's Sunday spills into the next month — a simple,
// deterministic rule that never double-counts or skips a week, unlike
// trying to split a straddling week across two monthly rollups.
func monthlyRollup(date time.Time) (rollupJob, bool) {
	if date.Day() != 1 {
		return rollupJob{}, false
	}
	monthEnd := date.AddDate(0, 0, -1)
	monthStart := time.Date(monthEnd.Year(), monthEnd.Month(), 1, 0, 0, 0, 0, monthEnd.Location())

	var sourcePeriods []string
	for d := monthStart; !d.After(monthEnd); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Monday {
			continue
		}
		year, week := d.ISOWeek()
		sourcePeriods = append(sourcePeriods, fmt.Sprintf("%04d-W%02d", year, week))
	}
	return rollupJob{
		level: "monthly", sourceLevel: "weekly",
		period:        monthStart.Format("2006-01"),
		sourcePeriods: sourcePeriods,
	}, true
}

// yearlyRollup fires on Jan 1, rolling up the 12 months of the year that
// just ended.
func yearlyRollup(date time.Time) (rollupJob, bool) {
	if date.Month() != time.January || date.Day() != 1 {
		return rollupJob{}, false
	}
	prevYear := date.Year() - 1
	sourcePeriods := make([]string, 12)
	for m := 1; m <= 12; m++ {
		sourcePeriods[m-1] = fmt.Sprintf("%04d-%02d", prevYear, m)
	}
	return rollupJob{
		level: "yearly", sourceLevel: "monthly",
		period:        fmt.Sprintf("%04d", prevYear),
		sourcePeriods: sourcePeriods,
	}, true
}
