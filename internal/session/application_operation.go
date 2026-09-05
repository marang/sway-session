package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

const (
	ApplicationOperationVersion  = 2
	MaxApplicationOperationItems = 128
	defaultOperationTTL          = 2 * time.Minute
	maxOperationTTL              = 10 * time.Minute
	applicationOperationDir      = "sway-session/app-operations"
	maxStoredOperations          = 256
	// A separately bounded scan lets upgraded processes prune the small
	// overflows that the pre-FD-lock implementation could leave behind.
	// The admission limit remains maxStoredOperations.
	maxStoredOperationRecoveryItems = maxStoredOperations * 4
	maxApplicationOperationSize     = 256 * 1024
	maxApplicationWorkspaceBytes    = 256
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
	ContextID       ContextID          `json:"context_id"`
	ContextRevision string             `json:"context_revision,omitempty"`
	Window          *WindowApplication `json:"window,omitempty"`
	DesktopID       string             `json:"desktop_id"`
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
			if operation.Kind == OperationRegister && item.ContextRevision != "" {
				return fmt.Errorf("items[%d]: registration must not contain a context revision", index)
			}
			if operation.Kind == OperationRebind {
				if err := validateSHA256("context revision", item.ContextRevision); err != nil {
					return fmt.Errorf("items[%d]: %w", index, err)
				}
			}
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
			if err := validateSHA256("context revision", item.ContextRevision); err != nil {
				return fmt.Errorf("items[%d]: %w", index, err)
			}
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

// ApplicationOperationContextRevision binds a pending context mutation to the
// launcher and compositor identity the user reviewed. Lifecycle-only changes
// such as desired-open and pinning deliberately do not invalidate approval.
func ApplicationOperationContextRevision(context Context) (string, error) {
	if err := context.Validate(); err != nil {
		return "", fmt.Errorf("validate application operation context: %w", err)
	}
	if context.App == nil {
		return "", errors.New("application operation context is not a desktop application")
	}
	revision := struct {
		ID       ContextID           `json:"id"`
		Launcher Launcher            `json:"launcher"`
		Identity ApplicationIdentity `json:"identity"`
	}{
		ID: context.ID, Launcher: context.Launcher, Identity: context.App.Identity,
	}
	encoded, err := json.Marshal(revision)
	if err != nil {
		return "", fmt.Errorf("encode application operation context revision: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ApplicationOperationStore persists one-time owner-only approval tokens.
type ApplicationOperationStore struct {
	RuntimeRoot string
	Now         func() time.Time
	Random      io.Reader
}

func (store ApplicationOperationStore) Create(operation ApplicationOperation) (string, error) {
	return store.CreateContext(context.Background(), operation)
}

// CreateContext is Create with cancelable operation-store lock acquisition.
func (store ApplicationOperationStore) CreateContext(ctx context.Context, operation ApplicationOperation) (string, error) {
	var token string
	err := statefile.WithPrivateDirectoryLockContext(ctx, store.directory(), func(directory *statefile.LockedPrivateDirectory) error {
		var err error
		token, err = store.createLocked(directory, operation)
		return err
	})
	return token, err
}

func (store ApplicationOperationStore) createLocked(directory *statefile.LockedPrivateDirectory, operation ApplicationOperation) (string, error) {
	now := store.now()
	if err := store.prune(directory, now); err != nil {
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
		err := directory.Create(token+".json", data)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("generate application operation token: too many collisions")
}

func (store ApplicationOperationStore) prune(directory *statefile.LockedPrivateDirectory, now time.Time) error {
	names, err := directory.List(maxStoredOperationRecoveryItems)
	if err != nil {
		return fmt.Errorf("inspect application operation store: %w", err)
	}
	remaining := len(names)
	for _, name := range names {
		token := strings.TrimSuffix(name, ".json")
		if name != token+".json" || !validOperationToken(token) {
			continue
		}
		data, readErr := directory.Read(name)
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
			if directory.Remove(name) == nil {
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
	return store.ConsumeContext(context.Background(), token)
}

// ConsumeContext is Consume with cancelable operation-store lock acquisition.
func (store ApplicationOperationStore) ConsumeContext(ctx context.Context, token string) (ApplicationOperation, error) {
	if !validOperationToken(token) {
		return ApplicationOperation{}, errors.New("application operation token must contain 32 lowercase hexadecimal characters")
	}
	data, err := statefile.ConsumePrivateFileContext(ctx, store.directory(), token+".json")
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

// Discard removes a stored approval which can no longer be presented. Missing
// tokens are already discarded and therefore succeed idempotently.
func (store ApplicationOperationStore) Discard(token string) error {
	return store.DiscardContext(context.Background(), token)
}

// DiscardContext is Discard with cancelable operation-store lock acquisition.
func (store ApplicationOperationStore) DiscardContext(ctx context.Context, token string) error {
	if !validOperationToken(token) {
		return errors.New("application operation token must contain 32 lowercase hexadecimal characters")
	}
	err := statefile.RemovePrivateFileContext(ctx, store.directory(), token+".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discard application operation token: %w", err)
	}
	return nil
}

// Active returns valid, unexpired approval operations without exposing their
// one-time tokens or consuming them. Concurrently consumed files are skipped.
func (store ApplicationOperationStore) Active() ([]ApplicationOperation, error) {
	return store.ActiveContext(context.Background())
}

// ActiveContext is Active with cancelable operation-store lock acquisition.
func (store ApplicationOperationStore) ActiveContext(ctx context.Context) ([]ApplicationOperation, error) {
	names, err := statefile.ListPrivateFilesContext(ctx, store.directory(), maxStoredOperationRecoveryItems)
	if errors.Is(err, os.ErrNotExist) {
		return []ApplicationOperation{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect application operation store: %w", err)
	}
	sort.Strings(names)
	now := store.now()
	active := make([]ApplicationOperation, 0, len(names))
	for _, name := range names {
		token := strings.TrimSuffix(name, ".json")
		if name != token+".json" || !validOperationToken(token) {
			continue
		}
		data, readErr := statefile.ReadPrivateFileContext(ctx, store.directory(), name)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read active application operation: %w", readErr)
		}
		if len(data) > maxApplicationOperationSize {
			return nil, fmt.Errorf("application operation exceeds %d bytes", maxApplicationOperationSize)
		}
		var operation ApplicationOperation
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&operation); err != nil {
			return nil, fmt.Errorf("decode active application operation: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, errors.New("active application operation contains trailing JSON")
			}
			return nil, err
		}
		if !operation.ExpiresAt.After(now) {
			removeErr := statefile.RemovePrivateFileContext(ctx, store.directory(), name)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return nil, fmt.Errorf("remove expired application operation: %w", removeErr)
			}
			continue
		}
		if err := operation.Validate(now); err != nil {
			return nil, fmt.Errorf("validate active application operation: %w", err)
		}
		active = append(active, operation)
	}
	return active, nil
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
