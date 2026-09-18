package selfcheck

import (
	"context"
	"errors"
	"testing"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// stubRetriever is a fake gateway.Retriever — Run/runOne only ever call
// Retrieve, so a fixed canned response (or error) is enough to exercise
// every branch without a real store or database.
type stubRetriever struct {
	result gateway.RetrievalResult
	err    error
}

func (s stubRetriever) Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message) (gateway.RetrievalResult, error) {
	return s.result, s.err
}

func TestRunPassesWhenExpectationsAreMet(t *testing.T) {
	r := stubRetriever{result: gateway.RetrievalResult{
		Gate:           gateway.GateFull,
		ContextMessage: "The project's working directory is /srv/hupi.",
	}}
	probes := []Probe{{ID: "p1", Query: "what's the working directory?", ExpectSubstring: "/srv/hupi", ExpectMinGate: "partial"}}

	results, err := Run(context.Background(), r, probes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("results = %+v, want exactly one passing result", results)
	}
	if results[0].Gate != gateway.GateFull {
		t.Errorf("Gate = %q, want %q", results[0].Gate, gateway.GateFull)
	}
}

func TestRunFailsWhenSubstringMissing(t *testing.T) {
	r := stubRetriever{result: gateway.RetrievalResult{Gate: gateway.GateFull, ContextMessage: "unrelated context"}}
	probes := []Probe{{ID: "p1", Query: "q", ExpectSubstring: "the expected fact"}}

	results, err := Run(context.Background(), r, probes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Passed {
		t.Error("expected the probe to fail when the expected substring is missing")
	}
}

func TestRunFailsWhenGateBelowMinimum(t *testing.T) {
	r := stubRetriever{result: gateway.RetrievalResult{Gate: gateway.GateSkipped}}
	probes := []Probe{{ID: "p1", Query: "q", ExpectMinGate: "full"}}

	results, err := Run(context.Background(), r, probes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Passed {
		t.Error("expected the probe to fail when the gate is below the minimum")
	}
}

func TestRunSubstringMatchIsCaseInsensitive(t *testing.T) {
	r := stubRetriever{result: gateway.RetrievalResult{ContextMessage: "The Working Directory is /srv/hupi."}}
	probes := []Probe{{ID: "p1", Query: "q", ExpectSubstring: "working directory"}}

	results, err := Run(context.Background(), r, probes)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Passed {
		t.Errorf("expected a case-insensitive substring match to pass, got: %s", results[0].Detail)
	}
}

func TestRunPropagatesRetrieverError(t *testing.T) {
	sentinel := errors.New("retriever exploded")
	r := stubRetriever{err: sentinel}
	probes := []Probe{{ID: "p1", Query: "q"}}

	_, err := Run(context.Background(), r, probes)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want the sentinel error wrapped", err)
	}
}

func TestRunDefaultsToIdentityDefaultScope(t *testing.T) {
	// A zero-value Probe.Scope must not be sent to Retrieve as-is —
	// runOne substitutes identity.DefaultScope for it.
	var gotActingUser, gotWorkspace identity.Scope
	r := recordingRetriever{
		stubRetriever: stubRetriever{result: gateway.RetrievalResult{}},
		onRetrieve: func(actingUser, workspace identity.Scope) {
			gotActingUser, gotWorkspace = actingUser, workspace
		},
	}
	probes := []Probe{{ID: "p1", Query: "q"}} // Scope left as the zero value

	if _, err := Run(context.Background(), r, probes); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotActingUser != identity.DefaultScope || gotWorkspace != identity.DefaultScope {
		t.Errorf("Retrieve called with {%+v, %+v}, want both = identity.DefaultScope", gotActingUser, gotWorkspace)
	}
}

func TestRunInvalidExpectMinGateErrors(t *testing.T) {
	r := stubRetriever{result: gateway.RetrievalResult{}}
	probes := []Probe{{ID: "p1", Query: "q", ExpectMinGate: "not-a-real-gate"}}

	if _, err := Run(context.Background(), r, probes); err == nil {
		t.Fatal("Run: expected an error for an invalid expect_min_gate value")
	}
}

type recordingRetriever struct {
	stubRetriever
	onRetrieve func(actingUser, workspace identity.Scope)
}

func (r recordingRetriever) Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message) (gateway.RetrievalResult, error) {
	r.onRetrieve(actingUser, workspace)
	return r.stubRetriever.Retrieve(ctx, actingUser, workspace, messages)
}
