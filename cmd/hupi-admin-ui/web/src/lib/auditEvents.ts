// Mirrors the exported Event* constants in internal/audit/audit.go.
// Hardcoded here deliberately (per the task brief) — this list changes
// rarely enough that round-tripping it through an API isn't worth it.
export const AUDIT_EVENT_TYPES = [
  "capture",
  "correct",
  "retrieve",
  "trace",
  "admin_provision",
  "admin_ui_view",
  "export",
  "import",
  "key_rotation",
] as const;
