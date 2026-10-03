package session

import (
	"encoding/json"
	"fmt"
)

const legacyLayoutSchemaVersion = 1

// layoutDocument retains the stored version and scratchpad field's presence
// until migration. Even an empty or null scratchpad field belongs to v2.
type layoutDocument struct {
	Version    int               `json:"version"`
	Workspaces []WorkspaceLayout `json:"workspaces"`
	Scratchpad json.RawMessage   `json:"scratchpad,omitempty"`
}

// decodeStoredLayoutSnapshot validates both row and payload versions before
// upgrading in memory. It never rewrites the stored payload or database schema.
func decodeStoredLayoutSnapshot(name string, encodingVersion int, payload []byte) (LayoutSnapshot, error) {
	if encodingVersion != legacyLayoutSchemaVersion && encodingVersion != LayoutSchemaVersion {
		return LayoutSnapshot{}, &UnsupportedVersionError{Document: name, Got: encodingVersion, Want: LayoutSchemaVersion}
	}
	var document layoutDocument
	if err := decodeDatabasePayload(name, payload, &document); err != nil {
		return LayoutSnapshot{}, err
	}
	if document.Version != legacyLayoutSchemaVersion && document.Version != LayoutSchemaVersion {
		return LayoutSnapshot{}, &UnsupportedVersionError{Document: name, Got: document.Version, Want: LayoutSchemaVersion}
	}
	if encodingVersion != document.Version {
		return LayoutSnapshot{}, fmt.Errorf("%s encoding_version %d does not match payload version %d", name, encodingVersion, document.Version)
	}
	return migrateLayoutDocument(name, document)
}

// migrateLayoutDocument is the common pure migration for SQLite and legacy
// JSON. Normal LayoutSnapshot validation continues to accept only current data.
func migrateLayoutDocument(name string, document layoutDocument) (LayoutSnapshot, error) {
	candidate := LayoutSnapshot{Version: LayoutSchemaVersion, Workspaces: document.Workspaces}
	switch document.Version {
	case legacyLayoutSchemaVersion:
		if len(document.Scratchpad) != 0 {
			return LayoutSnapshot{}, fmt.Errorf("%s version %d must not contain scratchpad data", name, document.Version)
		}
	case LayoutSchemaVersion:
		if len(document.Scratchpad) != 0 {
			if err := decodeDatabasePayload(name+" scratchpad", document.Scratchpad, &candidate.Scratchpad); err != nil {
				return LayoutSnapshot{}, err
			}
		}
	default:
		return LayoutSnapshot{}, &UnsupportedVersionError{Document: name, Got: document.Version, Want: LayoutSchemaVersion}
	}
	if err := candidate.Validate(); err != nil {
		return LayoutSnapshot{}, fmt.Errorf("validate %s: %w", name, err)
	}
	return candidate, nil
}
