-- docs/CODEBASE_SURVEY_AND_REVIEW.md finding B22: entity_relationships
-- (schema/0015) had no constraint stopping a relationship from being
-- recorded as "ending before it starts" — valid_from and valid_until are
-- both independently nullable (unknown start / still current), but
-- nothing tied them together when both are actually set.
--
-- A plain `check`, not `not null` on either column: null still means
-- exactly what schema/0015's own comments say (unknown start / still
-- current) — this only rejects the case where both are present and
-- backwards.
alter table entity_relationships
    add constraint entity_relationships_valid_date_order_check
    check (valid_from is null or valid_until is null or valid_from <= valid_until);
