package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newEpisodeID produces a time-sortable, collision-resistant-enough id: a
// millisecond timestamp prefix followed by random bytes. A real build
// should use a proper ULID library (e.g. oklog/ulid) for canonical
// Crockford base32 encoding and monotonic ordering within the same
// millisecond — this is a dependency-free stand-in for the sketch.
func newEpisodeID(t time.Time) string {
	var suffix [10]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("ep_%013d%s", t.UnixMilli(), hex.EncodeToString(suffix[:]))
}
