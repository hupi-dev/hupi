package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"hupi/internal/identity"
)

// TestRunScopesConcurrently_CountsFailuresWithoutRacing is the real
// regression test for the concurrency change this file's own doc comment
// describes: before it, this loop was purely sequential, so nothing
// here needed to be race-safe. Run with -race (same as every other
// concurrency-sensitive test in this repo) to actually catch a data
// race, not just check the final count.
func TestRunScopesConcurrently_CountsFailuresWithoutRacing(t *testing.T) {
	const n = 200
	scopes := make([]identity.Scope, n)
	for i := range scopes {
		scopes[i] = identity.Scope{Kind: identity.ScopeKindPrivate, Owner: fmt.Sprintf("user:test-concurrency-%d", i)}
	}

	var calls atomic.Int64
	failed := runScopesConcurrently(scopes, func(scope identity.Scope) error {
		calls.Add(1)
		return errors.New("every scope fails, deliberately")
	})

	if got := calls.Load(); got != n {
		t.Errorf("fn called %d times, want exactly %d (one per scope, none skipped or duplicated)", got, n)
	}
	if failed != n {
		t.Errorf("runScopesConcurrently reported %d failures, want %d (every call returned an error)", failed, n)
	}
}

// TestRunScopesConcurrently_OneFailureDoesNotStopOthers confirms the
// real reason fn's error is never handed back to the errgroup itself
// (see runScopesConcurrently's own doc comment): errgroup.Group with
// SetLimit cancels remaining work on the first non-nil error from any
// goroutine, which would silently reintroduce "one bad scope blocks
// everyone else" — exactly the regression this guards against.
func TestRunScopesConcurrently_OneFailureDoesNotStopOthers(t *testing.T) {
	const n = 50
	scopes := make([]identity.Scope, n)
	for i := range scopes {
		scopes[i] = identity.Scope{Kind: identity.ScopeKindPrivate, Owner: fmt.Sprintf("user:test-concurrency-%d", i)}
	}

	var calls atomic.Int64
	failed := runScopesConcurrently(scopes, func(scope identity.Scope) error {
		calls.Add(1)
		return errors.New("first scope fails")
	})

	if got := calls.Load(); got != n {
		t.Errorf("fn called %d times, want %d — a failure must not cancel the remaining scopes", got, n)
	}
	if failed != n {
		t.Errorf("failed = %d, want %d", failed, n)
	}
}

func TestRunScopesConcurrently_EmptyInput(t *testing.T) {
	failed := runScopesConcurrently(nil, func(identity.Scope) error {
		t.Fatal("fn should never be called for an empty scope list")
		return nil
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
}

func TestConsolidateConcurrency_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("HUPI_CONSOLIDATE_CONCURRENCY", "")
	if got := consolidateConcurrency(); got != 5 {
		t.Errorf("consolidateConcurrency() = %d, want default 5", got)
	}
}

func TestConsolidateConcurrency_UsesValidOverride(t *testing.T) {
	t.Setenv("HUPI_CONSOLIDATE_CONCURRENCY", "12")
	if got := consolidateConcurrency(); got != 12 {
		t.Errorf("consolidateConcurrency() = %d, want 12", got)
	}
}

func TestConsolidateConcurrency_FallsBackOnInvalidValue(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "-3"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("HUPI_CONSOLIDATE_CONCURRENCY", v)
			if got := consolidateConcurrency(); got != 5 {
				t.Errorf("consolidateConcurrency() with %q = %d, want fallback default 5", v, got)
			}
		})
	}
}
