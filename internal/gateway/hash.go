package gateway

import (
	"crypto/sha256"
	"encoding/hex"
)

// hashText produces the episode's dedup key (see MEMORY_FORMAT.md §
// Episode record) — sha256 over input+output so two copies of the same
// exchange, captured twice, are detectable without decrypting either.
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
