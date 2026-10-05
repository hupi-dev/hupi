-- Phase 2 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: source_count is
-- the reinforcement signal — a fact restated across several separate
-- mentions (internal/consolidation/contradiction.go's "redundant"
-- classification) increments this instead of being deleted as pure
-- duplication. memories (schema/0025) already has source_count; this is
-- the same column added to summary_key_facts, the table that
-- classification (and loadKeyFacts' own ranking) actually operates on.
alter table summary_key_facts add column source_count int not null default 1;
