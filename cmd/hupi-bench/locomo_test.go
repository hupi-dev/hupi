package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFormatLoCoMoTurnText_AppendsSharedImageTag is the real regression
// test for a measured gap (docs/BENCHMARKS.md's temporal-category trace,
// 2026-10-04): 31 of 54 zero-score temporal-category misses had an image
// on their evidence turn, and cmd/hupi-bench previously dropped LoCoMo's
// img_url/blip_caption fields entirely, so the model never saw any
// indication an image was even shared. Confirms the real conv-48
// "appreciation letter" turn now carries its caption using the exact
// same "[Shared image: %s]" tag convention production attachments use
// (internal/gateway/attachments.go), which consolidation already knows
// how to attribute (internal/consolidation/prompts.go's own doc
// comment).
func TestFormatLoCoMoTurnText_AppendsSharedImageTag(t *testing.T) {
	tn := locomoTurn{
		Speaker:     "Deborah",
		DiaID:       "D2:7",
		Text:        "Look what letter I received yesterday!",
		ImgURL:      []string{"https://i.redd.it/lr823iakg38b1.jpg"},
		BlipCaption: "a photo of a note written to someone",
	}
	got := formatLoCoMoTurnText(tn)
	for _, want := range []string{
		"Look what letter I received yesterday!",
		"[Shared image: lr823iakg38b1.jpg]",
		"a photo of a note written to someone",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatLoCoMoTurnText() = %q, want it to contain %q", got, want)
		}
	}
}

// TestFormatLoCoMoTurnText_NoImageReturnsTextUnchanged confirms the
// common case (the vast majority of turns, no image) is a pure no-op —
// this must never add a tag where there was no image.
func TestFormatLoCoMoTurnText_NoImageReturnsTextUnchanged(t *testing.T) {
	tn := locomoTurn{Speaker: "Deborah", Text: "Just a regular message."}
	if got := formatLoCoMoTurnText(tn); got != tn.Text {
		t.Errorf("formatLoCoMoTurnText() = %q, want unchanged %q", got, tn.Text)
	}
}

// TestFormatLoCoMoTurnText_MissingCaptionReturnsTextUnchanged guards
// against a malformed/partial record (an img_url with no caption) —
// better to silently skip the tag than fabricate an empty one.
func TestFormatLoCoMoTurnText_MissingCaptionReturnsTextUnchanged(t *testing.T) {
	tn := locomoTurn{Speaker: "Deborah", Text: "Look at this.", ImgURL: []string{"https://example.com/x.jpg"}}
	if got := formatLoCoMoTurnText(tn); got != tn.Text {
		t.Errorf("formatLoCoMoTurnText() = %q, want unchanged %q (no caption to attach)", got, tn.Text)
	}
}

func TestImageLabelFromURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://i.redd.it/lr823iakg38b1.jpg", "lr823iakg38b1.jpg"},
		{"no-slashes-here", "no-slashes-here"},
		{"https://example.com/path/", "https://example.com/path/"}, // trailing slash: no segment after it, falls back to the whole URL
	}
	for _, c := range cases {
		if got := imageLabelFromURL(c.url); got != c.want {
			t.Errorf("imageLabelFromURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestParseLoCoMoConversation_RealImageTurnCarriesCaption is a real,
// end-to-end regression test using conv-48's actual raw JSON shape (the
// exact traced case) — confirms the full parse path (not just the
// helper function in isolation) produces a turn whose text includes the
// image tag.
func TestParseLoCoMoConversation_RealImageTurnCarriesCaption(t *testing.T) {
	raw := locomoRaw{
		SampleID: "conv-48",
		Conversation: json.RawMessage(`{
			"session_2_date_time": "3:00 pm on 26 January, 2023",
			"session_2": [
				{"speaker": "Deborah", "dia_id": "D2:7", "text": "Look what letter I received yesterday!", "img_url": ["https://i.redd.it/lr823iakg38b1.jpg"], "blip_caption": "a photo of a note written to someone"}
			]
		}`),
	}
	conv, err := parseLoCoMoConversation(raw)
	if err != nil {
		t.Fatalf("parseLoCoMoConversation: %v", err)
	}
	if len(conv.sessions) != 1 || len(conv.sessions[0].turns) != 1 {
		t.Fatalf("got %d sessions, want 1 with 1 turn", len(conv.sessions))
	}
	got := conv.sessions[0].turns[0].text
	if !strings.Contains(got, "[Shared image:") || !strings.Contains(got, "a photo of a note written to someone") {
		t.Errorf("parsed turn text = %q, missing the shared-image tag/caption", got)
	}
}
