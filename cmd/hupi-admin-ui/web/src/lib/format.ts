// Go's zero time (0001-01-01T00:00:00Z) shows up in a couple of "create"
// response bodies (see docs handed down for POST /api/users — a known
// quirk, not the real CreatedAt). Render it as an em dash instead of a
// confusing year-1 date.
export function formatDate(iso: string | null | undefined): string {
  if (!iso || iso.startsWith("0001-01-01")) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}
