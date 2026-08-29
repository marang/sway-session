// Package session defines persistent Sway work-session identity and state.
package session

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	MarkPrefix  = "persist:"
	AppIDPrefix = "sway-session."
)

// ContextID is a canonical lowercase UUID used as immutable context identity.
type ContextID string

// ParseContextID validates and returns a canonical context UUID.
func ParseContextID(value string) (ContextID, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", errors.New("context ID must be a canonical UUID")
	}
	if value != strings.ToLower(value) {
		return "", errors.New("context ID must use lowercase hexadecimal")
	}
	hexValue := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(hexValue)
	if err != nil || len(decoded) != 16 {
		return "", errors.New("context ID must be a canonical UUID")
	}
	allZero := true
	for _, part := range decoded {
		if part != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "", errors.New("context ID must not be the nil UUID")
	}
	return ContextID(value), nil
}

// Validate checks that an ID already stored in a typed value is canonical.
func (id ContextID) Validate() error {
	_, err := ParseContextID(string(id))
	return err
}

// Mark returns the stable Sway mark for this context.
func (id ContextID) Mark() (string, error) {
	if err := id.Validate(); err != nil {
		return "", err
	}
	return MarkPrefix + string(id), nil
}

// AppID returns the provider-independent Wayland application ID for this context.
func (id ContextID) AppID() (string, error) {
	if err := id.Validate(); err != nil {
		return "", err
	}
	return AppIDPrefix + string(id), nil
}

// ParseAppID validates a managed-window application ID and extracts its
// context identity.
func ParseAppID(appID string) (ContextID, error) {
	if !strings.HasPrefix(appID, AppIDPrefix) {
		return "", fmt.Errorf("managed application ID must start with %q", AppIDPrefix)
	}
	return ParseContextID(strings.TrimPrefix(appID, AppIDPrefix))
}

// ParseMark validates a managed-window mark and extracts its context ID.
func ParseMark(mark string) (ContextID, error) {
	if !strings.HasPrefix(mark, MarkPrefix) {
		return "", fmt.Errorf("managed mark must start with %q", MarkPrefix)
	}
	return ParseContextID(strings.TrimPrefix(mark, MarkPrefix))
}

// UnmarshalJSON prevents invalid UUIDs from entering decoded persistent state.
func (id *ContextID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode context ID: %w", err)
	}
	parsed, err := ParseContextID(value)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
