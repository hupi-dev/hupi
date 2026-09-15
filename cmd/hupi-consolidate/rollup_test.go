package main

import (
	"reflect"
	"testing"
	"time"

	"hupi/internal/identity"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func TestWeeklyRollup_FiresOnMonday(t *testing.T) {
	// 2026-09-14 is a Monday; the week it just closed out (Mon 09-07
	// through Sun 09-13) is ISO week 37 — the same period name
	// MEMORY_FORMAT.md's own directory-layout example already uses.
	job, ok := weeklyRollup(mustDate(t, "2026-09-14"))
	if !ok {
		t.Fatal("expected a weekly rollup to be due")
	}
	if job.level != "weekly" || job.sourceLevel != "daily" {
		t.Errorf("got level=%s sourceLevel=%s", job.level, job.sourceLevel)
	}
	if job.period != "2026-W37" {
		t.Errorf("period = %q, want 2026-W37", job.period)
	}
	want := []string{
		"2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10",
		"2026-09-11", "2026-09-12", "2026-09-13",
	}
	if !reflect.DeepEqual(job.sourcePeriods, want) {
		t.Errorf("sourcePeriods = %v, want %v", job.sourcePeriods, want)
	}
}

func TestWeeklyRollup_SkipsNonMonday(t *testing.T) {
	if _, ok := weeklyRollup(mustDate(t, "2026-09-15")); ok {
		t.Error("expected no weekly rollup on a Tuesday")
	}
}

func TestMonthlyRollup_FiresOnFirstOfMonth(t *testing.T) {
	// September 2026 has exactly four Mondays (7, 14, 21, 28) — all four
	// ISO weeks are fully contained in September this particular month,
	// so this case doesn't exercise the straddling-week rule, only the
	// basic collection.
	job, ok := monthlyRollup(mustDate(t, "2026-10-01"))
	if !ok {
		t.Fatal("expected a monthly rollup to be due")
	}
	if job.level != "monthly" || job.sourceLevel != "weekly" {
		t.Errorf("got level=%s sourceLevel=%s", job.level, job.sourceLevel)
	}
	if job.period != "2026-09" {
		t.Errorf("period = %q, want 2026-09", job.period)
	}
	want := []string{"2026-W37", "2026-W38", "2026-W39", "2026-W40"}
	if !reflect.DeepEqual(job.sourcePeriods, want) {
		t.Errorf("sourcePeriods = %v, want %v", job.sourcePeriods, want)
	}
}

func TestMonthlyRollup_SkipsNonFirst(t *testing.T) {
	if _, ok := monthlyRollup(mustDate(t, "2026-09-15")); ok {
		t.Error("expected no monthly rollup mid-month")
	}
}

func TestYearlyRollup_FiresOnJanFirst(t *testing.T) {
	job, ok := yearlyRollup(mustDate(t, "2027-01-01"))
	if !ok {
		t.Fatal("expected a yearly rollup to be due")
	}
	if job.level != "yearly" || job.sourceLevel != "monthly" {
		t.Errorf("got level=%s sourceLevel=%s", job.level, job.sourceLevel)
	}
	if job.period != "2026" {
		t.Errorf("period = %q, want 2026", job.period)
	}
	want := []string{
		"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06",
		"2026-07", "2026-08", "2026-09", "2026-10", "2026-11", "2026-12",
	}
	if !reflect.DeepEqual(job.sourcePeriods, want) {
		t.Errorf("sourcePeriods = %v, want %v", job.sourcePeriods, want)
	}
}

func TestYearlyRollup_SkipsNonJanFirst(t *testing.T) {
	if _, ok := yearlyRollup(mustDate(t, "2026-12-01")); ok {
		t.Error("expected no yearly rollup on Dec 1")
	}
}

func TestDueRollups_JanFirstIsAlsoMonthlyAndWeeklyBoundary(t *testing.T) {
	// Jan 1 is always "the 1st" (monthly boundary) and, when it falls on
	// a Monday, a weekly boundary too — dueRollups must return every job
	// that's independently true, not just the first match.
	jan1Monday := mustDate(t, "2029-01-01")
	if jan1Monday.Weekday() != time.Monday {
		t.Fatalf("test fixture assumption wrong: 2029-01-01 is a %s, not Monday", jan1Monday.Weekday())
	}
	jobs := dueRollups(jan1Monday)
	var levels []string
	for _, j := range jobs {
		levels = append(levels, j.level)
	}
	want := []string{"weekly", "monthly", "yearly"}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("dueRollups levels = %v, want %v", levels, want)
	}
}

func TestDueRollups_OrdinaryDayIsEmpty(t *testing.T) {
	if jobs := dueRollups(mustDate(t, "2026-09-15")); len(jobs) != 0 {
		t.Errorf("expected no rollups due on an ordinary Tuesday, got %+v", jobs)
	}
}

// Sanity check that runDueRollups's scope fan-out doesn't itself need a
// database — it just calls Runner.RunRollup per (job, scope), and with
// zero jobs due it never touches the runner at all. The real
// database-backed exercise of RunRollup's idempotency guard lives in
// internal/consolidation's own test suite, not here.
func TestRunDueRollups_NoJobsDueIsNoop(t *testing.T) {
	scopes := []identity.Scope{{Kind: identity.ScopeKindPrivate, Owner: "user:unused"}}
	failed := runDueRollups(nil, nil, mustDate(t, "2026-09-15"), scopes)
	if failed != 0 {
		t.Errorf("expected 0 failures with no jobs due, got %d", failed)
	}
}
