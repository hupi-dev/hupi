-- Nothing tracks which embedding model actually produced a stored
-- `embedding` vector — found via a direct question about what happens
-- when active_embedding_provider changes: a vector from one model isn't
-- comparable to a vector from a different model (cosine similarity
-- between them is closer to noise than a real signal), and without this
-- column there's no way to even detect a mismatch, let alone fix it.
-- Existing rows get NULL here on this migration, which is exactly
-- "unknown/needs re-embedding" — see internal/reembed's doc comment.
alter table summaries add column embedding_model text;
alter table episodes  add column embedding_model text;
alter table entities  add column embedding_model text;

-- Gap Closure-style dedicated audit_log event type, same reasoning as
-- key_rotation getting one in schema/0011: "show me every re-embed ever
-- run on this scope" is a real question, distinct from a generic write.
alter table audit_log drop constraint audit_log_event_type_check;
alter table audit_log add constraint audit_log_event_type_check
    check (event_type in (
        'capture', 'correct', 'retrieve', 'trace',
        'admin_provision', 'admin_ui_view',
        'export', 'import',
        'key_rotation',
        'reembed'
    ));
