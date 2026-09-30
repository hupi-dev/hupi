package consolidation

import (
	"strings"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "clean JSON, nothing to strip",
			in:   `{"summary":"ok","key_facts":[]}`,
			want: `{"summary":"ok","key_facts":[]}`,
		},
		{
			name: "fenced with a json language tag",
			in:   "```json\n{\"summary\":\"ok\"}\n```",
			want: `{"summary":"ok"}`,
		},
		{
			name: "bare fence, no language tag",
			in:   "```\n{\"summary\":\"ok\"}\n```",
			want: `{"summary":"ok"}`,
		},
		{
			name: "leading and trailing prose around the object",
			in:   `Sure, here you go: {"summary":"ok"} Hope that helps!`,
			want: `{"summary":"ok"}`,
		},
		{
			name: "nested objects stay whole",
			in:   `{"a":{"b":1},"c":[{"d":2}]}`,
			want: `{"a":{"b":1},"c":[{"d":2}]}`,
		},
		{
			name: "trailing prose containing an unrelated brace no longer over-extends the span",
			in:   `{"summary":"ok"} note: curly braces like this one } show up in code samples too`,
			want: `{"summary":"ok"}`,
		},
		{
			name: "a brace inside a quoted string value doesn't miscount depth",
			in:   `{"summary":"uses { and } as block delimiters"}`,
			want: `{"summary":"uses { and } as block delimiters"}`,
		},
		{
			name: "an escaped quote inside a string doesn't end the string early",
			in:   `{"summary":"she said \"hi\" then closed with }"}`,
			want: `{"summary":"she said \"hi\" then closed with }"}`,
		},
		{
			name: "empty string falls back unchanged",
			in:   "",
			want: "",
		},
		{
			name: "no braces at all falls back unchanged",
			in:   "the model just refused and said sorry",
			want: "the model just refused and said sorry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.in)
			if got != tc.want {
				t.Errorf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildSummaryPromptIncludesSourceDateWhenPresent is
// docs/BENCHMARK_IMPROVEMENT_PLAN.md step 5's core check: a source with
// a known date must show it in the labeled block the consolidation
// model sees, since that's the only thing giving it anything to resolve
// a relative reference ("yesterday") against.
func TestBuildSummaryPromptIncludesSourceDateWhenPresent(t *testing.T) {
	sources := []textSource{
		{id: "ep_1", text: "I lost my job yesterday.", date: "2023-05-08"},
	}
	got := buildSummaryPrompt("daily", "2023-05-08", sources, "", nil)
	want := "--- id: ep_1 (date: 2023-05-08) ---"
	if !strings.Contains(got, want) {
		t.Errorf("buildSummaryPrompt() = %q, want it to contain %q", got, want)
	}
}

// TestBuildSummaryPromptOmitsDateLabelWhenAbsent confirms a source with
// no known date (a rollup source: a lower-level summary, which never
// sets textSource.date) falls back to the old, unlabeled format exactly
// as before this step — this is additive, not a requirement on every
// caller.
func TestBuildSummaryPromptOmitsDateLabelWhenAbsent(t *testing.T) {
	sources := []textSource{
		{id: "sum_1", text: "Weekly rollup content."},
	}
	got := buildSummaryPrompt("weekly", "2023-W19", sources, "", nil)
	if strings.Contains(got, "date:") {
		t.Errorf("buildSummaryPrompt() = %q, want no date label for a source with no known date", got)
	}
	if !strings.Contains(got, "--- id: sum_1 ---") {
		t.Errorf("buildSummaryPrompt() = %q, want the plain unlabeled format preserved", got)
	}
}
