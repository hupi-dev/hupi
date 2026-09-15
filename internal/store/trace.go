package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// Trace is the decrypted, human-readable answer to "what did this turn
// actually see" — the whole point of recording memory_gate and
// retrieved_refs on every episode (ARCHITECTURE.md § Retrieval
// observability) is that this question is answerable at all, rather than
// something you can only guess at from the model's response.
type Trace struct {
	Episode            TraceEpisode
	RetrievedSummaries []TraceSummary
	RetrievedEntities  []TraceEntity
	RetrievedEpisodes  []TraceEpisodeHit
}

type TraceEpisode struct {
	Scope         identity.Scope
	ID            string
	TS            time.Time
	MemoryGate    string
	InputText     string
	OutputText    string
	RetrievedRefs []identity.Ref
}

type TraceSummary struct {
	Ref              identity.Ref
	Period           string
	Level            string
	GroundingChecked bool
	Text             string
}

type TraceEntity struct {
	Ref        identity.Ref
	Kind       string
	Name       string
	Attributes string
}

// TraceEpisodeHit is a *different* episode this turn's vector search
// matched (docs/DESIGN_VS_BUILT.md #3) — not to be confused with
// TraceEpisode, which is the turn being traced itself.
type TraceEpisodeHit struct {
	Ref        identity.Ref
	InputText  string
	OutputText string
}

// Trace loads and decrypts one episode plus everything its retrieved_refs
// point at — used by cmd/hupi-trace, and by anyone auditing why a given
// turn recalled (or failed to recall) something. scope is the caller's own
// scope, checked against the episode's own recorded scope. Each ref is
// resolved in its *own* short transaction scoped to that ref's own
// Scope (docs/HARDENING_PLAN.md D1) — refs can legitimately span two
// scopes for one episode (e.g. a self_model ref from the acting user's
// private scope alongside workspace-scoped summary refs, per D3), so no
// single scope pair covers every ref the way it does for Retrieve's
// buildAnchor step.
//
// actor is recorded on the audit_log entry this writes (event_type
// "trace", docs/GAP_CLOSURE_PLAN.md §4.3) — cmd/hupi-trace's -actor flag.
// Investigating someone's memory content is exactly the kind of access
// the chosen audit level exists to record.
func (s *Store) Trace(ctx context.Context, scope identity.Scope, episodeID, actor string) (Trace, error) {
	var (
		ts                time.Time
		memoryGate        sql.NullString
		inputCT, outputCT []byte
		refsJSON          []byte
		keyVersion        int
	)
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select ts, memory_gate, input_text, output_text, retrieved_refs, key_version
			from episodes where id = $1 and scope_kind = $2 and scope_owner = $3
		`, episodeID, scope.Kind, scope.Owner).Scan(&ts, &memoryGate, &inputCT, &outputCT, &refsJSON, &keyVersion); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventTrace,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			TargetRef:      &identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: episodeID},
		})
	})
	if err != nil {
		return Trace{}, fmt.Errorf("store: load episode %s: %w", episodeID, err)
	}

	enc, err := s.keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		return Trace{}, fmt.Errorf("store: resolve encryption key for episode %s: %w", episodeID, err)
	}
	input, err := enc.Decrypt(inputCT)
	if err != nil {
		return Trace{}, fmt.Errorf("store: decrypt episode %s input_text: %w", episodeID, err)
	}
	output, err := enc.Decrypt(outputCT)
	if err != nil {
		return Trace{}, fmt.Errorf("store: decrypt episode %s output_text: %w", episodeID, err)
	}

	var refs []identity.Ref
	if err := json.Unmarshal(refsJSON, &refs); err != nil {
		return Trace{}, fmt.Errorf("store: parse retrieved_refs for episode %s: %w", episodeID, err)
	}

	trace := Trace{
		Episode: TraceEpisode{
			Scope:         scope,
			ID:            episodeID,
			TS:            ts,
			MemoryGate:    memoryGate.String,
			InputText:     input,
			OutputText:    output,
			RetrievedRefs: refs,
		},
	}

	for _, ref := range refs {
		switch ref.Kind {
		case identity.RefKindSummary:
			tsum, err := s.loadTraceSummary(ctx, ref)
			if err != nil {
				return Trace{}, err
			}
			trace.RetrievedSummaries = append(trace.RetrievedSummaries, tsum)
		case identity.RefKindEntity:
			te, err := s.loadTraceEntity(ctx, ref)
			if err != nil {
				return Trace{}, err
			}
			trace.RetrievedEntities = append(trace.RetrievedEntities, te)
		case identity.RefKindEpisode:
			teh, err := s.loadTraceEpisodeHit(ctx, ref)
			if err != nil {
				return Trace{}, err
			}
			trace.RetrievedEpisodes = append(trace.RetrievedEpisodes, teh)
		default:
			return Trace{}, fmt.Errorf("store: unknown ref kind %q for episode %s", ref.Kind, episodeID)
		}
	}

	return trace, nil
}

func (s *Store) loadTraceSummary(ctx context.Context, ref identity.Ref) (TraceSummary, error) {
	var period, level string
	var groundingChecked bool
	var summaryCT []byte
	var keyVersion int
	err := dbscope.Run(ctx, s.db, ref.Scope, ref.Scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select period, level, grounding_checked, summary, key_version
			from summaries where id = $1 and scope_kind = $2 and scope_owner = $3
		`, ref.ID, ref.Scope.Kind, ref.Scope.Owner).Scan(&period, &level, &groundingChecked, &summaryCT, &keyVersion)
	})
	if err != nil {
		return TraceSummary{}, fmt.Errorf("store: load referenced summary %s: %w", ref.ID, err)
	}
	enc, err := s.keys.GetVersion(ctx, ref.Scope, keyVersion)
	if err != nil {
		return TraceSummary{}, fmt.Errorf("store: resolve encryption key for summary %s: %w", ref.ID, err)
	}
	text, err := enc.Decrypt(summaryCT)
	if err != nil {
		return TraceSummary{}, fmt.Errorf("store: decrypt summary %s: %w", ref.ID, err)
	}
	return TraceSummary{Ref: ref, Period: period, Level: level, GroundingChecked: groundingChecked, Text: text}, nil
}

func (s *Store) loadTraceEpisodeHit(ctx context.Context, ref identity.Ref) (TraceEpisodeHit, error) {
	var inputCT, outputCT []byte
	var keyVersion int
	err := dbscope.Run(ctx, s.db, ref.Scope, ref.Scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select input_text, output_text, key_version from episodes
			where id = $1 and scope_kind = $2 and scope_owner = $3
		`, ref.ID, ref.Scope.Kind, ref.Scope.Owner).Scan(&inputCT, &outputCT, &keyVersion)
	})
	if err != nil {
		return TraceEpisodeHit{}, fmt.Errorf("store: load referenced episode %s: %w", ref.ID, err)
	}
	enc, err := s.keys.GetVersion(ctx, ref.Scope, keyVersion)
	if err != nil {
		return TraceEpisodeHit{}, fmt.Errorf("store: resolve encryption key for episode %s: %w", ref.ID, err)
	}
	input, err := enc.Decrypt(inputCT)
	if err != nil {
		return TraceEpisodeHit{}, fmt.Errorf("store: decrypt episode %s input_text: %w", ref.ID, err)
	}
	output, err := enc.Decrypt(outputCT)
	if err != nil {
		return TraceEpisodeHit{}, fmt.Errorf("store: decrypt episode %s output_text: %w", ref.ID, err)
	}
	return TraceEpisodeHit{Ref: ref, InputText: input, OutputText: output}, nil
}

func (s *Store) loadTraceEntity(ctx context.Context, ref identity.Ref) (TraceEntity, error) {
	var kind, name string
	var attrsCT []byte
	var keyVersion int
	err := dbscope.Run(ctx, s.db, ref.Scope, ref.Scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select kind, name, attributes, key_version from entities
			where id = $1 and scope_kind = $2 and scope_owner = $3
		`, ref.ID, ref.Scope.Kind, ref.Scope.Owner).Scan(&kind, &name, &attrsCT, &keyVersion)
	})
	if err != nil {
		return TraceEntity{}, fmt.Errorf("store: load referenced entity %s: %w", ref.ID, err)
	}
	enc, err := s.keys.GetVersion(ctx, ref.Scope, keyVersion)
	if err != nil {
		return TraceEntity{}, fmt.Errorf("store: resolve encryption key for entity %s: %w", ref.ID, err)
	}
	attrs, err := enc.Decrypt(attrsCT)
	if err != nil {
		return TraceEntity{}, fmt.Errorf("store: decrypt entity %s attributes: %w", ref.ID, err)
	}
	return TraceEntity{Ref: ref, Kind: kind, Name: name, Attributes: attrs}, nil
}
