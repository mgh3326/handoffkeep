package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// DecisionRef is the id the operator sees: dr-2615-1 reads D-2615-1.
func DecisionRef(requestID string) string {
	return "D-" + strings.TrimPrefix(requestID, "dr-")
}

// DecisionRequestVer is the first 8 hex digits of the SHA-256 of what was asked.
// The JSON encoding keeps field boundaries, so moving text between the question
// and an option changes the hash.
func DecisionRequestVer(question string, options DecisionOptions, doc string, refs []string) string {
	allRefs := []string{}
	if doc != "" {
		allRefs = append(allRefs, doc)
	}
	allRefs = append(allRefs, refs...)
	encoded, err := json.Marshal(struct {
		Question string           `json:"question"`
		Options  []DecisionOption `json:"options"`
		Refs     []string         `json:"refs"`
	}{question, options.Options, allRefs})
	if err != nil {
		// Strings and plain structs always marshal.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])[:8]
}

// The retry schedule of a Discord delivery (decision notices, task threads):
// exponential from 30s, capped at an hour; each caller sets its own attempt
// limit.
const (
	deliveryBaseBackoff   = 30 * time.Second
	deliveryMaxBackoff    = time.Hour
	deliveryErrorMaxBytes = 500
)

// deliveryBackoff is the wait after a failed delivery attempt, given the
// attempts made before it.
func deliveryBackoff(attempts int) time.Duration {
	return min(deliveryBaseBackoff<<min(attempts, 8), deliveryMaxBackoff)
}

// firstBytes cuts s to at most max bytes on a rune boundary.
func firstBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
