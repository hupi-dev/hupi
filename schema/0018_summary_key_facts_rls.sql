-- docs/CODEBASE_SURVEY_AND_REVIEW.md finding B18: summary_key_facts was
-- deliberately left without RLS or its own scope columns
-- (schema/0005_hardening_phase3_rls.sql's own comment) on the
-- assumption that it would only ever be reached by joining through
-- summaries. That assumption no longer held even at the time of this
-- review: six call sites query it directly by summary_id, relying
-- entirely on the caller having already verified the parent summary's
-- scope first, with nothing independently enforcing that if a future
-- caller ever got the sequencing wrong — exactly the condition that
-- comment's own "if a direct query path is ever added, revisit this"
-- was written for.
--
-- Backfilled from each row's real parent summary (summary_id has a
-- real, non-nullable foreign key into summaries, on delete cascade —
-- every existing row genuinely has exactly one parent to backfill from,
-- no orphans possible), not a flat default: unlike the original
-- Tier-3 migration (schema/0002), a blanket "every existing row is
-- scope X" default would be wrong now that real multi-scope data
-- exists.
alter table summary_key_facts add column scope_kind text;
alter table summary_key_facts add column scope_owner text;

update summary_key_facts skf
set scope_kind = s.scope_kind, scope_owner = s.scope_owner
from summaries s
where skf.summary_id = s.id;

alter table summary_key_facts alter column scope_kind set not null;
alter table summary_key_facts alter column scope_owner set not null;
alter table summary_key_facts
    add constraint summary_key_facts_scope_kind_check check (scope_kind in ('private', 'shared'));

create index summary_key_facts_scope_idx on summary_key_facts (scope_kind, scope_owner);

-- Same policy shape as episodes/summaries/entities (schema/0005's own
-- comment there explains the two-scope-pair USING/WITH CHECK split).
-- Every current call site already runs inside a correctly-scoped
-- dbscope.Run transaction and only ever reaches a row it already
-- independently verified belongs to that scope — this is a second,
-- independent layer underneath that, not a behavior change for any of
-- them. It only ever matters for a future caller that gets the
-- sequencing wrong, which is exactly the point.
alter table summary_key_facts enable row level security;

create policy scope_isolation on summary_key_facts
    for all
    using (
        (scope_kind = current_setting('hupi.acting_scope_kind', true)
         and scope_owner = current_setting('hupi.acting_scope_owner', true))
        or
        (scope_kind = current_setting('hupi.workspace_scope_kind', true)
         and scope_owner = current_setting('hupi.workspace_scope_owner', true))
    )
    with check (
        scope_kind = current_setting('hupi.workspace_scope_kind', true)
        and scope_owner = current_setting('hupi.workspace_scope_owner', true)
    );
