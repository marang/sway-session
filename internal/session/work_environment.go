package session

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EnvironmentID identifies organization metadata, never a managed window.
type EnvironmentID string

func NewEnvironmentID() (EnvironmentID, error) {
	id, err := NewContextID()
	return EnvironmentID(id), err
}

func ParseEnvironmentID(value string) (EnvironmentID, error) {
	id, err := ParseContextID(value)
	if err != nil {
		return "", fmt.Errorf("environment ID: %w", err)
	}
	return EnvironmentID(id), nil
}

func (id EnvironmentID) Validate() error {
	_, err := ParseEnvironmentID(string(id))
	return err
}

func (id *EnvironmentID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := ParseEnvironmentID(value)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// WorkEnvironment references contexts without owning their lifecycle or placement.
// Members retain presentation order; Primary may be empty. Missing contexts are
// valid references and remain visible until an explicit membership edit.
type WorkEnvironment struct {
	ID       EnvironmentID `json:"id"`
	Name     string        `json:"name"`
	Revision int64         `json:"revision"`
	Members  []ContextID   `json:"members"`
	Primary  ContextID     `json:"primary,omitempty"`
}

func ValidateWorkEnvironmentName(name string) error {
	if name == "" {
		return errors.New("environment name must not be empty")
	}
	return validateMetadata("environment name", name)
}

func (environment WorkEnvironment) Validate() error {
	if err := environment.ID.Validate(); err != nil {
		return err
	}
	if err := ValidateWorkEnvironmentName(environment.Name); err != nil {
		return err
	}
	if environment.Revision <= 0 {
		return errors.New("environment revision must be positive")
	}
	seen := make(map[ContextID]bool, len(environment.Members))
	for _, id := range environment.Members {
		if err := id.Validate(); err != nil {
			return err
		}
		if seen[id] {
			return fmt.Errorf("duplicate environment member %s", id)
		}
		seen[id] = true
	}
	if environment.Primary != "" && !seen[environment.Primary] {
		return errors.New("environment primary must be a member")
	}
	return nil
}
