-- docs/CODEBASE_SURVEY_AND_REVIEW.md finding B14: Runner.Correct's "not
-- already superseded" guard and its actual write (storeSummary) are
-- separate transactions with real work in between (reloading sources,
-- a real grounding-check LLM call) - two concurrent correctors of the
-- same target (an operator's manual correction racing the automatic
-- contradiction detector, or two different triggering runs both
-- re-consolidating/correcting the same older summary) could both pass
-- the guard and both successfully insert a new row with the same
-- supersedes value, forking history: two rows both claiming to replace
-- the same target, with no principled way for retrieval to pick
-- between them.
--
-- A partial unique index (not a full one - supersedes is null for
-- every normal, non-correcting summary, and there's nothing wrong with
-- many of those) makes the second concurrent writer's INSERT fail with
-- a real, visible constraint violation instead of silently succeeding.
-- Scoped per (scope_kind, scope_owner, supersedes): two different
-- scopes correcting unrelated summaries must never contend with each
-- other.
--
-- Known migration risk, deliberately not silently worked around: if a
-- real, already-deployed scope already has two rows both superseding
-- the same target (the exact fork this fix prevents going forward),
-- this CREATE UNIQUE INDEX will fail outright rather than applying
-- partially - which is the right failure mode (a human needs to look
-- at and resolve a real, pre-existing fork, not have one silently
-- picked for them by migration order).
create unique index summaries_supersedes_unique_idx
    on summaries (scope_kind, scope_owner, supersedes)
    where supersedes is not null;
