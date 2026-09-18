package consolidation

import "testing"

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
