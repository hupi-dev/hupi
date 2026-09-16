package consolidation

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
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
}

// ConsolidationOutput is the consolidation LLM's structured response
// shape, and also what a human-authored correction (Runner.Correct)
// supplies directly instead of an LLM generating it.
type ConsolidationOutput struct {
	Summary         string          `json:"summary"`
	KeyFacts        []KeyFactOutput `json:"key_facts"`
	EntitiesTouched []EntityUpdate  `json:"entities_touched"`
}
