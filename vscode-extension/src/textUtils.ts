// Small text helpers shared by any feature that asks the model for raw
// code back (inline edit, inline completions, multi-file edit) — kept
// free of `vscode` so it's trivially unit-testable.

/** Models asked for "just the code" still sometimes wrap it in a markdown
 *  fence anyway — strip one if present, otherwise return the text as-is. */
export function stripCodeFences(text: string): string {
  const trimmed = text.trim();
  const fenced = trimmed.match(/^```[^\n]*\n([\s\S]*?)\n?```$/);
  return fenced ? fenced[1] : trimmed;
}
