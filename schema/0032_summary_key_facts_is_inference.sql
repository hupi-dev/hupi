-- Phase 4 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: consolidation-
-- time inference extraction. memories (schema/0025) already has
-- is_inference; this is the same column added to summary_key_facts,
-- the table loadKeyFacts actually reads from.
alter table summary_key_facts add column is_inference boolean not null default false;
