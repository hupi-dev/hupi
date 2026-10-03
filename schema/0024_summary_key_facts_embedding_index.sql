-- internal/store/retrieve.go's fusedSearchSummaries gained a third
-- candidate-selection signal: a per-fact nearest-neighbor query over
-- summary_key_facts.embedding, catching a summary whose overall
-- embedding/keyword profile doesn't match a query at all but which
-- contains one specific, correctly-grounded fact that does (a real,
-- confirmed gap — see docs/LONGMEMEVAL_ACCURACY_PLAN.md's 852ce960
-- case). schema/0021_summary_key_facts_embeddings.sql deliberately
-- skipped an index here, on the grounds that "retrieval never does a
-- cross-summary nearest-neighbor search over facts... with no query to
-- serve" — that's no longer true, so this reopens that decision.
--
-- Same partial-HNSW shape as episodes_embedding_idx (schema/0001_init.sql)
-- — WHERE embedding IS NOT NULL only, no scope/grounded predicate in the
-- index itself; the query applies those as a post-filter, the same
-- accepted tradeoff episodes' own vector search already lives with.
--
-- CONCURRENTLY: this table can be large (schema/0021's own comment notes
-- 100+ facts on a single busy day), and building a non-concurrent index
-- would hold a lock blocking writes for the duration of the build. This
-- statement must be the only one in its migration file — CREATE INDEX
-- CONCURRENTLY cannot run inside a transaction block, and schema/migrate.sh
-- applies each file via a plain `psql -f` with no surrounding transaction.
create index concurrently if not exists summary_key_facts_embedding_idx
  on summary_key_facts using hnsw (embedding vector_cosine_ops)
  where embedding is not null;
