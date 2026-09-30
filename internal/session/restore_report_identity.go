package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// RestoreContextDigest binds diagnostic evidence to the identity and launcher
// originally requested, without duplicating private paths in the report.
func RestoreContextDigest(context Context) string {
	value := struct {
		Launcher Launcher             `json:"launcher"`
		Identity *ApplicationIdentity `json:"identity,omitempty"`
	}{Launcher: context.Launcher}
	if context.App != nil {
		value.Identity = &context.App.Identity
	}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
