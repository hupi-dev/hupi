-- Phase 1 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: native fact
-- expiration ("I have a dentist appointment tomorrow" shouldn't still
-- read as current state a year later). memories (schema/0025) already
-- has expires_at/expire_reason — this is the same pair added to
-- summary_key_facts, the table loadKeyFacts/exhaustiveKeyFactsForEntity
-- actually read facts from (memories only mirrors a live citation
-- pointer, per internal/consolidation/store.go's storeSummary comment;
-- it isn't itself a second place expiration needs enforcing, since
-- nothing queries memories for ranked retrieval today — schema/0025's
-- own doc comment on why no ANN index was added there either).
alter table summary_key_facts add column expires_at date;
alter table summary_key_facts add column expire_reason text;
