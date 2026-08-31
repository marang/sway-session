package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
)

const (
	ApplicationOperationVersion  = 1
	MaxApplicationOperationItems = 128
	defaultOperationTTL          = 2 * time.Minute
	maxOperationTTL              = 10 * time.Minute
	applicationOperationDir      = "sway-session/app-operations"
	maxStoredOperations          = 256
	maxApplicationOperationSize  = 256 * 1024
	maxApplicationWorkspaceBytes = 256
)

type ApplicationOperationKind string

const (
	OperationRegister  ApplicationOperationKind = "register"
	OperationRebind    ApplicationOperationKind = "rebind"
	OperationReapprove ApplicationOperationKind = "reapprove"
)

// ApplicationOperation is a short-lived typed approval request. It contains
// no executable command, arguments, environment, title, URI, or private app
// state.
type ApplicationOperation struct {
	Version   int                        `json:"version"`
	Kind      ApplicationOperationKind   `json:"kind"`
	ExpiresAt time.Time                  `json:"expires_at"`
	Items     []ApplicationOperationItem `json:"items"`
}

type ApplicationOperationItem struct {
	ContextID ContextID          `json:"context_id"`
	Window    *WindowApplication `json:"window,omitempty"`
	DesktopID string             `json:"desktop_id"`
}

func (operation *ApplicationOperation) Validate(now time.Time) error {
	if operation == nil {
		return errors.New("application operation is nil")
	}
	if operation.Version != ApplicationOperationVersion {
		return fmt.Errorf("unsupported application operation version %d", operation.Version)
	}
	switch operation.Kind {
	case OperationRegister, OperationRebind, OperationReapprove:
	default:
		return fmt.Errorf("unsupported application operation kind %q", operation.Kind)
	}
	if operation.ExpiresAt.IsZero() || !operation.ExpiresAt.After(now) || operation.ExpiresAt.After(now.Add(maxOperationTTL)) {
		return errors.New("application operation expiry is invalid or expired")
	}
	if len(operation.Items) == 0 || len(operation.Items) > MaxApplicationOperationItems {
		return fmt.Errorf("application operation must contain between 1 and %d items", MaxApplicationOperationItems)
	}
	seenContexts := make(map[ContextID]struct{}, len(operation.Items))
	seenContainers := make(map[int64]struct{}, len(operation.Items))
	for index := range operation.Items {
		item := &operation.Items[index]
		if err := item.ContextID.Validate(); err != nil {
			return fmt.Errorf("items[%d]: invalid context ID: %w", index, err)
		}
		if _, duplicate := seenContexts[item.ContextID]; duplicate {
			return fmt.Errorf("items[%d]: duplicate context ID", index)
		}
		seenContexts[item.ContextID] = struct{}{}
		if err := validateDesktopID(item.DesktopID); err != nil {
			return fmt.Errorf("items[%d]: %w", index, err)
		}
		switch operation.Kind {
		case OperationRegister, OperationRebind:
			if item.Window == nil {
				return fmt.Errorf("items[%d]: operation requires window evidence", index)
			}
			if item.Window.ContainerID <= 0 || !validApplicationWorkspace(item.Window.Workspace) {
				return fmt.Errorf("items[%d]: invalid application window", index)
			}
			if err := item.Window.Identity.validate(); err != nil {
				return fmt.Errorf("items[%d]: invalid application identity: %w", index, err)
			}
			if len(item.Window.ContextMarks) > 1 {
				return fmt.Errorf("items[%d]: application window has multiple persistent context marks", index)
			}
			for _, id := range item.Window.ContextMarks {
				if err := id.Validate(); err != nil {
					return fmt.Errorf("items[%d]: invalid persistent context mark: %w", index, err)
				}
			}
			if operation.Kind == OperationRegister && len(item.Window.ContextMarks) != 0 {
				return fmt.Errorf("items[%d]: registration window is already persistently marked", index)
			}
			if operation.Kind == OperationRebind && len(item.Window.ContextMarks) == 1 && item.Window.ContextMarks[0] != item.ContextID {
				return fmt.Errorf("items[%d]: rebind window belongs to another persistent context", index)
			}
			if _, duplicate := seenContainers[item.Window.ContainerID]; duplicate {
				return fmt.Errorf("items[%d]: duplicate container ID", index)
			}
			seenContainers[item.Window.ContainerID] = struct{}{}
		case OperationReapprove:
			if item.Window != nil {
				return fmt.Errorf("items[%d]: reapproval must not contain window evidence", index)
			}
		}
	}
	if operation.Kind != OperationRegister && len(operation.Items) != 1 {
		return errors.New("rebind and reapprove operations require exactly one item")
	}
	return nil
}

// ApplicationOperationStore persists one-time owner-only approval tokens.
type ApplicationOperationStore struct {
	RuntimeRoot string
	Now         func() time.Time
	Random      io.Reader
}

func (store ApplicationOperationStore) Create(operation ApplicationOperation) (string, error) {
	now := store.now()
	if err := store.prune(now); err != nil {
		return "", err
	}
	operation.Version = ApplicationOperationVersion
	if operation.ExpiresAt.IsZero() {
		operation.ExpiresAt = now.Add(defaultOperationTTL)
	}
	if err := operation.Validate(now); err != nil {
		return "", err
	}
	data, err := json.Marshal(operation)
	if err != nil {
		return "", err
	}
	if len(data) > maxApplicationOperationSize {
		return "", fmt.Errorf("application operation exceeds %d bytes", maxApplicationOperationSize)
	}
	for range 16 {
		var entropy [16]byte
		if _, err := io.ReadFull(store.random(), entropy[:]); err != nil {
			return "", fmt.Errorf("generate application operation token: %w", err)
		}
		token := hex.EncodeToString(entropy[:])
		err := statefile.CreatePrivateFile(store.directory(), token+".json", data)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("generate application operation token: too many collisions")
}

func (store ApplicationOperationStore) prune(now time.Time) error {
	names, err := statefile.ListPrivateFiles(store.directory(), maxStoredOperations)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect application operation store: %w", err)
	}
	remaining := len(names)
	for _, name := range names {
		token := strings.TrimSuffix(name, ".json")
		if name != token+".json" || !validOperationToken(token) {
			continue
		}
		data, readErr := statefile.ReadPrivateFile(store.directory(), name)
		if readErr != nil {
			continue
		}
		if len(data) > maxApplicationOperationSize {
			continue
		}
		var envelope struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		if json.Unmarshal(data, &envelope) == nil && !envelope.ExpiresAt.After(now) {
			if statefile.RemovePrivateFile(store.directory(), name) == nil {
				remaining--
			}
		}
	}
	if remaining >= maxStoredOperations {
		return fmt.Errorf("application operation store already contains the maximum of %d entries", maxStoredOperations)
	}
	return nil
}

func DefaultApplicationOperationStore() (ApplicationOperationStore, error) {
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) || filepath.Clean(runtimeRoot) != runtimeRoot {
		return ApplicationOperationStore{}, errors.New("XDG_RUNTIME_DIR must be a clean absolute path")
	}
	return ApplicationOperationStore{RuntimeRoot: runtimeRoot}, nil
}

func (store ApplicationOperationStore) Consume(token string) (ApplicationOperation, error) {
	if !validOperationToken(token) {
		return ApplicationOperation{}, errors.New("application operation token must contain 32 lowercase hexadecimal characters")
	}
	data, err := statefile.ConsumePrivateFile(store.directory(), token+".json")
	if err != nil {
		return ApplicationOperation{}, fmt.Errorf("consume application operation token: %w", err)
	}
	if len(data) > maxApplicationOperationSize {
		return ApplicationOperation{}, fmt.Errorf("application operation exceeds %d bytes", maxApplicationOperationSize)
	}
	var operation ApplicationOperation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&operation); err != nil {
		return ApplicationOperation{}, fmt.Errorf("decode application operation: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ApplicationOperation{}, errors.New("application operation contains trailing JSON")
		}
		return ApplicationOperation{}, err
	}
	if err := operation.Validate(store.now()); err != nil {
		return ApplicationOperation{}, err
	}
	return operation, nil
}

func (store ApplicationOperationStore) directory() string {
	return filepath.Join(store.RuntimeRoot, applicationOperationDir)
}

func (store ApplicationOperationStore) now() time.Time {
	if store.Now != nil {
		return store.Now().UTC()
	}
	return time.Now().UTC()
}

func (store ApplicationOperationStore) random() io.Reader {
	if store.Random != nil {
		return store.Random
	}
	return rand.Reader
}

func validOperationToken(token string) bool {
	if len(token) != 32 || token != strings.ToLower(token) {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == 16
}

func validApplicationWorkspace(workspace string) bool {
	if workspace == "" || workspace == "__i3_scratch" || workspace == RestoreStagingWorkspace || len(workspace) > maxApplicationWorkspaceBytes || strings.TrimSpace(workspace) != workspace {
		return false
	}
	for _, character := range workspace {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
