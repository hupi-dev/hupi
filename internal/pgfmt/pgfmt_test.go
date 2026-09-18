package pgfmt

import (
	"reflect"
	"testing"
)

func TestTextArray(t *testing.T) {
	cases := []struct {
		name  string
		items []string
		want  string
	}{
		{"empty slice", []string{}, "{}"},
		{"nil slice", nil, "{}"},
		{"single item", []string{"a"}, `{"a"}`},
		{"multiple items", []string{"a", "b", "c"}, `{"a","b","c"}`},
		{"escapes a literal quote", []string{`say "hi"`}, `{"say \"hi\""}`},
		{"escapes a literal backslash", []string{`a\b`}, `{"a\\b"}`},
		{"backslash before an escaped quote is escaped first, not confused with it", []string{`\"`}, `{"\\\""}`},
		{"item containing a comma", []string{"a,b", "c"}, `{"a,b","c"}`},
		{"empty string item", []string{""}, `{""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TextArray(tc.items); got != tc.want {
				t.Errorf("TextArray(%q) = %q, want %q", tc.items, got, tc.want)
			}
		})
	}
}

func TestParseTextArray(t *testing.T) {
	cases := []struct {
		name    string
		literal string
		want    []string
	}{
		{"empty array", "{}", nil},
		{"single item", `{"a"}`, []string{"a"}},
		{"multiple items", `{"a","b","c"}`, []string{"a", "b", "c"}},
		{"unescapes a literal quote", `{"say \"hi\""}`, []string{`say "hi"`}},
		{"unescapes a literal backslash", `{"a\\b"}`, []string{`a\b`}},
		{"item containing a comma", `{"a,b","c"}`, []string{"a,b", "c"}},
		{"empty string item", `{""}`, []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTextArray(tc.literal)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseTextArray(%q) = %#v, want %#v", tc.literal, got, tc.want)
			}
		})
	}
}

// TestTextArrayRoundTrip is the property this pair of functions actually
// promises (ParseTextArray's own doc comment: "reverses TextArray") —
// checked directly, for inputs a hand-written case list could easily
// forget to cover (e.g. an id that happens to contain a comma or a
// backslash-quote sequence).
func TestTextArrayRoundTrip(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"a"},
		{"a", "b", "c"},
		{"a,b", "c"},
		{`quoted "value"`, `back\slash`, `both\"together`},
		{"", "b", ""},
		{"trailing,comma,"},
	}
	for _, items := range cases {
		t.Run("", func(t *testing.T) {
			literal := TextArray(items)
			got := ParseTextArray(literal)
			want := items
			if len(want) == 0 {
				want = nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip of %#v through %q = %#v, want %#v", items, literal, got, want)
			}
		})
	}
}

func TestNullable(t *testing.T) {
	if got := Nullable(""); got != nil {
		t.Errorf("Nullable(\"\") = %#v, want nil", got)
	}
	if got := Nullable("x"); got != "x" {
		t.Errorf("Nullable(\"x\") = %#v, want %q", got, "x")
	}
}

func TestVectorLiteral(t *testing.T) {
	cases := []struct {
		name string
		v    []float32
		want string
	}{
		{"empty", []float32{}, "[]"},
		{"single value", []float32{0.5}, "[0.5]"},
		{"multiple values", []float32{0.1, -0.2, 3}, "[0.1,-0.2,3]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VectorLiteral(tc.v); got != tc.want {
				t.Errorf("VectorLiteral(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
