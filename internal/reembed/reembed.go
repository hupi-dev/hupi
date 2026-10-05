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
	"errors"
	"fmt"
	"strings"

	"hupi/internal/audit"
	"hupi/internal/consolidation"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
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
	KeyFactsPending  int
}

func (s ScopeStatus) Total() int {
	return s.SummariesPending + s.EpisodesPending + s.EntitiesPending + s.KeyFactsPending
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
		// Only grounded facts of current (non-superseded) summaries are
		// ever retrieved (internal/store's loadKeyFacts, internal/
		// consolidation's checkCrossPeriodContradictions) — an ungrounded
		// fact or one belonging to a summary a later correction replaced
		// is out of scope for the same reason an episode below the
		// importance threshold is out of scope for episode re-embedding:
		// it's never pending, because it's never going to be read.
		if err := tx.QueryRowContext(ctx, `
			select count(*) from summary_key_facts f
			where f.grounded
			  and (f.embedding is null or f.embedding_model is distinct from $1)
			  and f.scope_kind = $2 and f.scope_owner = $3
			  and not exists (select 1 from summaries newer where newer.supersedes = f.summary_id)
		`, model, scope.Kind, scope.Owner).Scan(&st.KeyFactsPending); err != nil {
			return fmt.Errorf("count pending key facts: %w", err)
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

	n, err = r.reembedKeyFactBatch(ctx, scope, model, batchSize)
	if err != nil {
		return 0, "", false, fmt.Errorf("reembed: key facts for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if n > 0 {
		return n, "summary_key_facts", false, nil
	}

	return 0, "", true, nil
}

// decryptWithRetry resolves id's key_version and decrypts every
// ciphertext in ciphertexts (ciphertextCols names the column each one
// came from, same order — episodes pass both input_text and output_text
// together here, since they always share one row's key_version and must
// be refreshed together if either turns out stale, not independently).
//
// keyVersion/ciphertexts are whatever this row's batch SELECT originally
// read, which can go stale: internal/rotate may migrate the row to a new
// version — re-encrypting its ciphertext and bumping key_version together
// in one transaction — and an operator may prune the old version, any
// time between that SELECT and this call
// (docs/CODEBASE_SURVEY_AND_REVIEW.md B19). When that happens,
// KeyStore.GetVersion fails with ErrKeyVersionNotFound even though the
// row is perfectly readable — just under a newer version.
//
// Critically, a stale key_version means the ciphertext this batch already
// has in memory is stale too: rotate's migration re-encrypts a row's
// ciphertext and bumps its key_version together, in the same transaction,
// so the two can never drift apart from each other independently — only
// together, from the perspective of a reader whose own SELECT predates
// both. A first version of this fix re-read only the key_version on
// retry, still decrypting against the original stale ciphertext — which
// reliably fails (a real AES-GCM auth-tag mismatch, not silent
// corruption, but still a failure, not the recovery this is meant to
// provide). So retrying re-reads the ciphertext columns alongside the
// current key_version, together, and decrypts that fresh pair — never a
// version from one moment paired with ciphertext from an earlier one.
func (r *Runner) decryptWithRetry(ctx context.Context, scope identity.Scope, table string, id any, keyVersion int, ciphertextCols []string, ciphertexts [][]byte) ([]string, error) {
	enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
	if err == nil {
		return decryptEach(enc, ciphertexts)
	}
	if !errors.Is(err, crypto.ErrKeyVersionNotFound) {
		return nil, err
	}

	fresh := make([][]byte, len(ciphertexts))
	dest := make([]any, len(fresh))
	for i := range fresh {
		dest[i] = &fresh[i]
	}
	current, rerr := r.refetchRow(ctx, scope, table, id, ciphertextCols, dest...)
	if rerr != nil || current == keyVersion {
		return nil, err
	}
	freshEnc, ferr := r.keys.GetVersion(ctx, scope, current)
	if ferr != nil {
		return nil, err
	}
	return decryptEach(freshEnc, fresh)
}

func decryptEach(enc *crypto.Encryptor, ciphertexts [][]byte) ([]string, error) {
	texts := make([]string, len(ciphertexts))
	for i, ct := range ciphertexts {
		text, err := enc.Decrypt(ct)
		if err != nil {
			return nil, err
		}
		texts[i] = text
	}
	return texts, nil
}

// refetchRow re-reads id's current key_version plus whatever ciphertext
// columns the caller names (scanning them into dest, same order), inside
// a single query — so the version and the ciphertext it decrypts come
// from the same, current row state, never a version from one moment and
// ciphertext from an earlier one.
func (r *Runner) refetchRow(ctx context.Context, scope identity.Scope, table string, id any, ciphertextCols []string, dest ...any) (int, error) {
	var version int
	cols := strings.Join(ciphertextCols, ", ") + ", key_version"
	scanDest := append(append([]any{}, dest...), &version)
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, fmt.Sprintf(`select %s from %s where id = $1`, cols, table), id).Scan(scanDest...)
	})
	return version, err
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
		texts, err := r.decryptWithRetry(ctx, scope, "summaries", rr.id, rr.keyVersion, []string{"summary"}, [][]byte{rr.summaryCT})
		if err != nil {
			return 0, fmt.Errorf("decrypt summary %s: %w", rr.id, err)
		}
		text := texts[0]
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(text)}})
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
		texts, err := r.decryptWithRetry(ctx, scope, "episodes", rr.id, rr.keyVersion, []string{"input_text", "output_text"}, [][]byte{rr.inputCT, rr.outputCT})
		if err != nil {
			return 0, fmt.Errorf("decrypt episode %s: %w", rr.id, err)
		}
		input, output := texts[0], texts[1]
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(consolidation.EpisodeEmbedText(input, output))}})
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
	}
	var rows []row
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, kind, name from entities
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
			if err := dbRows.Scan(&rr.id, &rr.kind, &rr.name); err != nil {
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
		// Unlike decryptWithRetry's own race (a batch-selected ciphertext
		// paired with a key_version that's since moved on), this reads
		// content and key_version together, fresh, right here — no gap
		// between the read and the decrypt for this row to race against,
		// so no retry is needed the way the other entity columns still
		// use it above.
		var attrs map[string]string
		err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			var attrErr error
			attrs, attrErr = entityattrs.Current(ctx, tx, r.keys, scope, rr.id)
			return attrErr
		})
		if err != nil {
			return 0, fmt.Errorf("load current attributes for entity %s: %w", rr.id, err)
		}
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(consolidation.EntityEmbedText(rr.kind, rr.name, attrs))}})
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

// reembedKeyFactBatch is the one table in this package embedded in a
// single batched Embed call per batch rather than once per row — facts
// are short (summarySystemPrompt asks for "a single concrete, checkable
// fact"), so even a full batchSize's worth comfortably fits one request,
// the same reasoning internal/consolidation's own embedKeyFacts already
// applies at write time. Unlike that best-effort write-time call, a
// mismatched vector count here is a hard error, matching this package's
// own established style (a single bad row already blocks its whole batch
// on retry, same as every other table) rather than silently skipping the
// batch.
func (r *Runner) reembedKeyFactBatch(ctx context.Context, scope identity.Scope, model string, batchSize int) (int, error) {
	type row struct {
		id         int64
		factCT     []byte
		keyVersion int
	}
	var rows []row
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select f.id, f.fact, f.key_version from summary_key_facts f
			where f.grounded
			  and (f.embedding is null or f.embedding_model is distinct from $1)
			  and f.scope_kind = $2 and f.scope_owner = $3
			  and not exists (select 1 from summaries newer where newer.supersedes = f.summary_id)
			order by f.id limit $4
		`, model, scope.Kind, scope.Owner, batchSize)
		if err != nil {
			return fmt.Errorf("query key facts: %w", err)
		}
		defer dbRows.Close()
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.factCT, &rr.keyVersion); err != nil {
				return fmt.Errorf("scan key fact: %w", err)
			}
			rows = append(rows, rr)
		}
		return dbRows.Err()
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	texts := make([]string, len(rows))
	for i, rr := range rows {
		decrypted, err := r.decryptWithRetry(ctx, scope, "summary_key_facts", rr.id, rr.keyVersion, []string{"fact"}, [][]byte{rr.factCT})
		if err != nil {
			return 0, fmt.Errorf("decrypt key fact %d: %w", rr.id, err)
		}
		texts[i] = provider.TruncateForEmbedding(decrypted[0])
	}

	resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: texts})
	if err != nil {
		return 0, fmt.Errorf("embed key facts batch: %w", err)
	}
	if len(resp.Vectors) != len(rows) {
		return 0, fmt.Errorf("embed key facts batch: embedder returned %d vectors, want %d", len(resp.Vectors), len(rows))
	}

	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		for i, rr := range rows {
			if _, err := tx.ExecContext(ctx, `
				update summary_key_facts set embedding = $1::vector, embedding_model = $2 where id = $3
			`, pgfmt.VectorLiteral(resp.Vectors[i]), model, rr.id); err != nil {
				return fmt.Errorf("write embedding for key fact %d: %w", rr.id, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}
