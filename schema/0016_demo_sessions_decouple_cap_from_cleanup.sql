-- docs/CODEBASE_SURVEY_AND_REVIEW.md finding A11: the demo's daily
-- session cap (CreateSession, internal/demo/store.go) counts
-- demo_sessions rows created in the last 24 hours, but
-- guest_user_id's "on delete cascade" meant deleting the guest's users
-- row (cmd/hupi-demo-sweep, every ~15 min, as soon as a session passes
-- its 3-hour TTL) cascaded away the demo_sessions row itself —
-- immediately, not after 24 hours. A session stopped counting toward
-- the daily cap roughly 3 hours after creation instead of 24, making the
-- real achievable daily session volume (and the real aggregate API
-- spend the cap exists to bound) roughly 8x the configured limit.
--
-- Changed to "on delete set null": deleting the guest's users row (and
-- all their real conversation data, still done promptly at TTL — this
-- migration doesn't change when that cleanup happens) now leaves the
-- demo_sessions row itself behind, with guest_user_id null, purely so it
-- keeps counting toward the 24-hour cap window until internal/demo's
-- own Sweep separately removes the now-empty row once that window
-- closes (see Sweep's updated two-phase doc comment).
alter table demo_sessions alter column guest_user_id drop not null;
alter table demo_sessions drop constraint demo_sessions_guest_user_id_fkey;
alter table demo_sessions add constraint demo_sessions_guest_user_id_fkey
    foreign key (guest_user_id) references users(id) on delete set null;
