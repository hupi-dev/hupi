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

// TestSummarySystemPromptCoversAssistantGeneratedContent is a real
// regression test for the live LongMemEval failure this prompt change
// exists to fix (89527b6b): a children's story the assistant wrote,
// including a Plesiosaur's blue scaly body, was dropped wholesale from
// its day's summary — summarySystemPrompt's only criterion was a
// "concrete, checkable fact," implicitly framed around real-world
// user-reported facts, with no natural home for assistant-written
// creative content. Asserts the specific instructions this fix adds, not
// just that the prompt changed.
func TestSummarySystemPromptCoversAssistantGeneratedContent(t *testing.T) {
	for _, want := range []string{
		"a story, poem, children's book, plan, itinerary, recipe",
		`"assistant:" line is assistant-written`,
		"You are recording what was written, not judging whether it is true",
	} {
		if !strings.Contains(summarySystemPrompt, want) {
			t.Errorf("summarySystemPrompt missing expected guidance: %q", want)
		}
	}
}

// TestPerEpisodeFactPromptCoversAssistantGeneratedContent is
// perEpisodeFactPrompt's own counterpart to the test above — the
// per-episode pass has the identical narrow-criteria problem
// summarySystemPrompt had, so it needs the same broadening.
func TestPerEpisodeFactPromptCoversAssistantGeneratedContent(t *testing.T) {
	for _, want := range []string{
		"a specific detail inside something the assistant produced for the user",
		`"assistant:" line is assistant-written`,
		"list its distinctive specific details",
	} {
		if !strings.Contains(perEpisodeFactPrompt, want) {
			t.Errorf("perEpisodeFactPrompt missing expected guidance: %q", want)
		}
	}
}

// TestSummarySystemPromptAndPerEpisodeFactPromptWarnAgainstUnsupportedTemporalFraming
// is a real regression test for 852ce960's grounding-stage rejection:
// direct, repeated testing against the real grounding model showed it
// consistently rejects "The user got pre-approved for $400,000 from
// Wells Fargo in a previous conversation" (3/3 trials false — the
// isolated exchange has no actual earlier conversation to confirm that
// framing against) while accepting the same fact stated plainly, "The
// user was pre-approved for $400,000 from Wells Fargo" (3/3 trials
// true). Both extraction prompts need the same instruction: a user
// stating a fact via "remember when I...?" is a real, direct statement
// of that fact, and extracting it shouldn't add an inferred
// "previously"/"earlier conversation" qualifier the source itself never
// states.
func TestSummarySystemPromptAndPerEpisodeFactPromptWarnAgainstUnsupportedTemporalFraming(t *testing.T) {
	for name, prompt := range map[string]string{
		"summarySystemPrompt":  summarySystemPrompt,
		"perEpisodeFactPrompt": perEpisodeFactPrompt,
	} {
		for _, want := range []string{
			"remember when I got pre-approved for $400,000 from Wells Fargo",
			`do not add a qualifier like "previously," "in an earlier conversation," or "as mentioned before"`,
		} {
			if !strings.Contains(strings.ToLower(prompt), strings.ToLower(want)) {
				t.Errorf("%s missing expected guidance: %q", name, want)
			}
		}
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
