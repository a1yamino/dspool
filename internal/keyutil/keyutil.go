// Package keyutil holds small helpers for identifying and masking API keys.
package keyutil

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint returns a stable, non-reversible identifier for a key.
// It is safe to persist in the state file.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}

// Hash returns the full SHA-256 hex digest of a token. Used for client auth
// lookups so the plaintext token is not kept in a lookup map.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Mask renders a key for logs and the admin endpoint.
func Mask(secret string) string {
	if len(secret) <= 12 {
		return "****"
	}
	return secret[:3] + "…" + secret[len(secret)-4:]
}
