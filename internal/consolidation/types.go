package consolidation

import "encoding/json"

// textSource is a labeled block of text to summarize — an episode's
// USER/ASSISTANT transcript for a daily rollup, or a lower-level summary's
// text for a weekly/monthly/yearly rollup. Both cases feed the same
// generateSummary/groundingCheck machinery.
type textSource struct {
	id   string
	text string
}

// KeyFactOutput is what the consolidation LLM is asked to produce per
// fact — see prompts.go's summarySystemPrompt. SourceEpisodeIDs is the
// model's own citation, independently re-checked by groundingCheck rather
// than trusted outright. Exported because Runner.Correct takes a
// ConsolidationOutput from outside this package (cmd/hupi-correct).
type KeyFactOutput struct {
	Fact             string   `json:"fact"`
	SourceEpisodeIDs []string `json:"source_episode_ids"`
}

// EntityUpdate is one entity a consolidation run or correction says this
// period touched. Normal consolidation (RunDaily/RunRollup) merges this
// over the entity's existing attributes (see upsertEntities /
// mergeAttributes); Runner.Correct instead replaces them wholesale — see
// upsertEntities' doc comment for why those need to differ. Because of
// that, a hand-authored correction should list every attribute the
// entity should still have, not just the one that changed — Runner's
// CurrentContent produces exactly that starting point (cmd/hupi-correct's
// -dump-template).
type EntityUpdate struct {
	// ID is a hint, not the final identity: storeSummary overwrites it
	// with canonicalEntityID(Kind, Name, ID) before ever touching the
	// database, so the same real-world entity lands on the same row
	// however its id string is spelled from one run to the next. Kept as
	// an input field (rather than dropped) so a caller reading back
	// CurrentContent's dump sees the id its correction needs to keep
	// referring to.
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
}

// entityUpdateWire mirrors EntityUpdate field-for-field except
// Attributes, which is deliberately json.RawMessage-valued rather than
// string-valued — see UnmarshalJSON below for why.
type entityUpdateWire struct {
	ID         string                     `json:"id"`
	Kind       string                     `json:"kind"`
	Name       string                     `json:"name"`
	Attributes map[string]json.RawMessage `json:"attributes"`
}

// UnmarshalJSON accepts any JSON value for an attribute, not only a
// plain string — a real, reproduced failure mode: the consolidation
// prompt asks for `"attributes": {"key": "value"}` (string values), but
// a model asked for a naturally boolean/list-shaped attribute (e.g.
// "is_vegetarian" or "hobbies") sometimes emits a native JSON bool or
// array there instead of a stringified one. Rejecting the whole day's
// consolidation over one attribute's shape (the original behavior —
// json.Unmarshal into map[string]string fails outright on a non-string
// value) is worse than losslessly falling back to that value's own
// compact JSON text form when it isn't already a plain string.
func (e *EntityUpdate) UnmarshalJSON(data []byte) error {
	var wire entityUpdateWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	e.ID = wire.ID
	e.Kind = wire.Kind
	e.Name = wire.Name
	if len(wire.Attributes) == 0 {
		e.Attributes = nil
		return nil
	}
	e.Attributes = make(map[string]string, len(wire.Attributes))
	for k, v := range wire.Attributes {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			e.Attributes[k] = s
			continue
		}
		e.Attributes[k] = string(v)
	}
	return nil
}

// ConsolidationOutput is the consolidation LLM's structured response
// shape, and also what a human-authored correction (Runner.Correct)
// supplies directly instead of an LLM generating it.
type ConsolidationOutput struct {
	Summary         string          `json:"summary"`
	KeyFacts        []KeyFactOutput `json:"key_facts"`
	EntitiesTouched []EntityUpdate  `json:"entities_touched"`
}
