// Package reembed implements bulk re-embedding of already-stored content
// after the active embedding provider changes —
// schema/0013_embedding_model_tracking.sql records which model produced
// each summary/episode/entity embedding specifically so this package can
// find every row whose vector no longer matches the currently configured
// provider and regenerate it. Without this, switching providers would
// silently and undetectably corrupt vector search over every
// pre-existing row: cosine similarity between vectors from two different
// embedding models is closer to noise than a real signal, but nothing
// short of this package's own query can tell an old, now-incomparable
// vector apart from a current one.
//
// Unlike internal/rotate (its closest sibling: both are online, resumable,
// per-scope batch migrations), this package keeps no persisted cursor or
// state table. It doesn't need one: "needs re-embedding" is exactly
// `embedding is null or embedding_model is distinct from <current>`, and a
// row this package has already re-embedded no longer matches that
// predicate — so a crash and restart simply re-issues the same query and
// picks up whatever's left, with no cursor to get out of sync with the
// data. internal/rotate's cursor exists purely for its own `-status`
// reporting (see its doc comment); here, Status answers that by running
// the same live count query Continue's batches use, so there's nothing to
// persist even for that.
package reembed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"hupi/internal/audit"
	"hupi/internal/consolidation"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

type Runner struct {
	db       *sql.DB
	keys     *crypto.KeyStore
	embedder provider.Provider
}

func New(db *sql.DB, keys *crypto.KeyStore, embedder provider.Provider) *Runner {
	return &Runner{db: db, keys: keys, embedder: embedder}
}

// ScopeStatus is a live snapshot of how many rows in scope still need
// re-embedding under the currently configured model — always recomputed,
// never persisted (see package doc comment).
type ScopeStatus struct {
	Model            string
	SummariesPending int
	EpisodesPending  int
	EntitiesPending  int
}

func (s ScopeStatus) Total() int {
	return s.SummariesPending + s.EpisodesPending + s.EntitiesPending
}

// Status counts, per table, how many of scope's rows are either missing
// an embedding or carry one from a model other than the one currently
// configured. Episodes are counted under the same
// consolidation.EpisodeEmbedImportanceThreshold bar embedHighImportanceEpisodes
// writes under — an episode that was never supposed to be embedded isn't
// "pending", it's out of scope for this table entirely.
func (r *Runner) Status(ctx context.Context, scope identity.Scope) (ScopeStatus, error) {
	model := consolidation.EmbedderIdentity(r.embedder)
	st := ScopeStatus{Model: model}
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select count(*) from summaries
			where (embedding is null or embedding_model is distinct from $1)
			  and scope_kind = $2 and scope_owner = $3
		`, model, scope.Kind, scope.Owner).Scan(&st.SummariesPending); err != nil {
			return fmt.Errorf("count pending summaries: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `
			select count(*) from episodes
			where type = 'interaction' and importance >= $1
			  and (embedding is null or embedding_model is distinct from $2)
			  and scope_kind = $3 and scope_owner = $4
		`, consolidation.EpisodeEmbedImportanceThreshold, model, scope.Kind, scope.Owner).Scan(&st.EpisodesPending); err != nil {
			return fmt.Errorf("count pending episodes: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `
			select count(*) from entities
			where (embedding is null or embedding_model is distinct from $1)
			  and scope_kind = $2 and scope_owner = $3
		`, model, scope.Kind, scope.Owner).Scan(&st.EntitiesPending); err != nil {
			return fmt.Errorf("count pending entities: %w", err)
		}
		return nil
	})
	return st, err
}

// Continue processes up to batchSize rows of whatever in scope still
// needs re-embedding — summaries first, then episodes, then entities,
// stopping at the first table with anything to do — and returns how many
// rows it changed. done is true only once all three tables have nothing
// pending. Safe to call in a loop until done, and safe to call again
// after a crash or restart (see package doc comment for why nothing needs
// resuming from a saved position).
//
// A single row that can never be re-embedded (e.g. corrupted ciphertext)
// blocks its whole batch on retry, same as internal/rotate's batches —
// not a new failure mode this package introduces.
func (r *Runner) Continue(ctx context.Context, scope identity.Scope, batchSize int) (processed int, table string, done bool, err error) {
	model := consolidation.EmbedderIdentity(r.embedder)

	n, err := r.reembedSummaryBatch(ctx, scope, model, batchSize)
	if err != nil {
		return 0, "", false, fmt.Errorf("reembed: summaries for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if n > 0 {
		return n, "summaries", false, nil
	}

	n, err = r.reembedEpisodeBatch(ctx, scope, model, batchSize)
	if err != nil {
		return 0, "", false, fmt.Errorf("reembed: episodes for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if n > 0 {
		return n, "episodes", false, nil
	}

	n, err = r.reembedEntityBatch(ctx, scope, model, batchSize)
	if err != nil {
		return 0, "", false, fmt.Errorf("reembed: entities for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if n > 0 {
		return n, "entities", false, nil
	}

	return 0, "", true, nil
}

// LogRun writes one audit_log entry summarizing a completed re-embed
// pass — call this once, after a Continue loop has returned done=true,
// with the sum of every processed count from that loop (skip the call
// entirely if that sum is 0: there's nothing to report). There's no
// persisted per-scope state to attach this to the way
// internal/rotate.Start/markCompleted do, so unlike that package this is
// the caller's responsibility to invoke, not something Continue triggers
// on its own.
func (r *Runner) LogRun(ctx context.Context, scope identity.Scope, actor string, processed int) error {
	model := consolidation.EmbedderIdentity(r.embedder)
	return dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventReembed,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			Detail:         map[string]any{"rows_reembedded": processed, "model": model},
		})
	})
}

func (r *Runner) reembedSummaryBatch(ctx context.Context, scope identity.Scope, model string, batchSize int) (int, error) {
	type row struct {
		id         string
		summaryCT  []byte
		keyVersion int
	}
	var rows []row
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, summary, key_version from summaries
			where (embedding is null or embedding_model is distinct from $1)
			  and scope_kind = $2 and scope_owner = $3
			order by id limit $4
		`, model, scope.Kind, scope.Owner, batchSize)
		if err != nil {
			return fmt.Errorf("query summaries: %w", err)
		}
		defer dbRows.Close()
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.summaryCT, &rr.keyVersion); err != nil {
				return fmt.Errorf("scan summary: %w", err)
			}
			rows = append(rows, rr)
		}
		return dbRows.Err()
	})
	if err != nil {
		return 0, err
	}

	for _, rr := range rows {
		enc, err := r.keys.GetVersion(ctx, scope, rr.keyVersion)
		if err != nil {
			return 0, fmt.Errorf("resolve encryption key for summary %s: %w", rr.id, err)
		}
		text, err := enc.Decrypt(rr.summaryCT)
		if err != nil {
			return 0, fmt.Errorf("decrypt summary %s: %w", rr.id, err)
		}
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{text}})
		if err != nil {
			return 0, fmt.Errorf("embed summary %s: %w", rr.id, err)
		}
		if len(resp.Vectors) == 0 {
			return 0, fmt.Errorf("embedder returned no vectors for summary %s", rr.id)
		}
		vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				update summaries set embedding = $1::vector, embedding_model = $2 where id = $3
			`, vectorLiteral, model, rr.id)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("write embedding for summary %s: %w", rr.id, err)
		}
	}
	return len(rows), nil
}

func (r *Runner) reembedEpisodeBatch(ctx context.Context, scope identity.Scope, model string, batchSize int) (int, error) {
	type row struct {
		id                string
		inputCT, outputCT []byte
		keyVersion        int
	}
	var rows []row
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, input_text, output_text, key_version from episodes
			where type = 'interaction' and importance >= $1
			  and (embedding is null or embedding_model is distinct from $2)
			  and scope_kind = $3 and scope_owner = $4
			order by id limit $5
		`, consolidation.EpisodeEmbedImportanceThreshold, model, scope.Kind, scope.Owner, batchSize)
		if err != nil {
			return fmt.Errorf("query episodes: %w", err)
		}
		defer dbRows.Close()
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.inputCT, &rr.outputCT, &rr.keyVersion); err != nil {
				return fmt.Errorf("scan episode: %w", err)
			}
			rows = append(rows, rr)
		}
		return dbRows.Err()
	})
	if err != nil {
		return 0, err
	}

	for _, rr := range rows {
		enc, err := r.keys.GetVersion(ctx, scope, rr.keyVersion)
		if err != nil {
			return 0, fmt.Errorf("resolve encryption key for episode %s: %w", rr.id, err)
		}
		input, err := enc.Decrypt(rr.inputCT)
		if err != nil {
			return 0, fmt.Errorf("decrypt episode %s input_text: %w", rr.id, err)
		}
		output, err := enc.Decrypt(rr.outputCT)
		if err != nil {
			return 0, fmt.Errorf("decrypt episode %s output_text: %w", rr.id, err)
		}
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{consolidation.EpisodeEmbedText(input, output)}})
		if err != nil {
			return 0, fmt.Errorf("embed episode %s: %w", rr.id, err)
		}
		if len(resp.Vectors) == 0 {
			return 0, fmt.Errorf("embedder returned no vectors for episode %s", rr.id)
		}
		vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				update episodes set embedding = $1::vector, embedding_model = $2 where id = $3
			`, vectorLiteral, model, rr.id)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("write embedding for episode %s: %w", rr.id, err)
		}
	}
	return len(rows), nil
}

func (r *Runner) reembedEntityBatch(ctx context.Context, scope identity.Scope, model string, batchSize int) (int, error) {
	type row struct {
		id         string
		kind, name string
		attrsCT    []byte
		keyVersion int
	}
	var rows []row
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, kind, name, attributes, key_version from entities
			where (embedding is null or embedding_model is distinct from $1)
			  and scope_kind = $2 and scope_owner = $3
			order by id limit $4
		`, model, scope.Kind, scope.Owner, batchSize)
		if err != nil {
			return fmt.Errorf("query entities: %w", err)
		}
		defer dbRows.Close()
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.kind, &rr.name, &rr.attrsCT, &rr.keyVersion); err != nil {
				return fmt.Errorf("scan entity: %w", err)
			}
			rows = append(rows, rr)
		}
		return dbRows.Err()
	})
	if err != nil {
		return 0, err
	}

	for _, rr := range rows {
		enc, err := r.keys.GetVersion(ctx, scope, rr.keyVersion)
		if err != nil {
			return 0, fmt.Errorf("resolve encryption key for entity %s: %w", rr.id, err)
		}
		attrsJSON, err := enc.Decrypt(rr.attrsCT)
		if err != nil {
			return 0, fmt.Errorf("decrypt entity %s attributes: %w", rr.id, err)
		}
		var attrs map[string]string
		if attrsJSON != "" {
			if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
				return 0, fmt.Errorf("parse entity %s attributes: %w", rr.id, err)
			}
		}
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{consolidation.EntityEmbedText(rr.kind, rr.name, attrs)}})
		if err != nil {
			return 0, fmt.Errorf("embed entity %s: %w", rr.id, err)
		}
		if len(resp.Vectors) == 0 {
			return 0, fmt.Errorf("embedder returned no vectors for entity %s", rr.id)
		}
		vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				update entities set embedding = $1::vector, embedding_model = $2
				where id = $3 and scope_kind = $4 and scope_owner = $5
			`, vectorLiteral, model, rr.id, scope.Kind, scope.Owner)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("write embedding for entity %s: %w", rr.id, err)
		}
	}
	return len(rows), nil
}
