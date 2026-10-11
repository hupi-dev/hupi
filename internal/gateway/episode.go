package gateway

import "time"

// NewInteractionEpisode fills the fields every captured interaction needs
// (type, provider, texts, importance heuristic, dedup hash), so every
// capture path builds episodes the same way consolidation expects.
func NewInteractionEpisode(id string, ts time.Time, vendor, model, input, output string) Episode {
	return Episode{
		ID:             id,
		TS:             ts,
		Type:           "interaction",
		ProviderVendor: vendor,
		ProviderModel:  model,
		InputText:      input,
		OutputText:     output,
		Importance:     estimateImportance(input, output),
		Hash:           hashText(input + "\x00" + output),
	}
}
