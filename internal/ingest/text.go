package ingest

import "strings"

// textReplacementRatioLimit rejects a "text file" whose content is
// mostly replacement characters after UTF-8 repair — a strong signal
// the upload is actually a mislabeled binary, not a text file saved
// under a legacy encoding with a few bad bytes. Feeding mostly-garbage
// text into the capture/consolidation/embedding pipeline is worse than
// a clear upfront rejection.
const textReplacementRatioLimit = 0.05

func extractText(data []byte) (Result, error) {
	s := string(data)
	if len(s) == 0 {
		return Result{}, nil
	}
	repaired := strings.ToValidUTF8(s, "�")
	if ratio := replacementRatio(repaired); ratio > textReplacementRatioLimit {
		return Result{}, &InvalidTextError{ReplacementRatio: ratio}
	}
	return Result{Text: repaired}, nil
}

// InvalidTextError is returned when a plain-text attachment is mostly
// unrepairable garbage — the caller maps this to a 400, not a 500, the
// same way a malformed JSON body is a client error, not a server one.
type InvalidTextError struct {
	ReplacementRatio float64
}

func (e *InvalidTextError) Error() string {
	return "ingest: content is not valid text (looks like a mislabeled binary file)"
}

func replacementRatio(s string) float64 {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	bad := 0
	for _, r := range runes {
		if r == '�' {
			bad++
		}
	}
	return float64(bad) / float64(len(runes))
}
