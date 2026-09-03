package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/marang/sway-title-animator/internal/statefile"
	"github.com/marang/sway-title-animator/internal/swayipc"
)

type TerminalOpenAction string

const (
	TerminalActionCreated  TerminalOpenAction = "created"
	TerminalActionReused   TerminalOpenAction = "reused"
	TerminalActionOpened   TerminalOpenAction = "opened"
	TerminalActionAttached TerminalOpenAction = "attached"
	TerminalActionFocused  TerminalOpenAction = "focused"
	TerminalActionNoChange TerminalOpenAction = "no_change"
)

type TerminalOpenRequest struct {
	New         bool
	ContextID   ContextID
	Identity    TerminalIdentity
	Adapter     TerminalAdapter
	Cwd         string
	CwdExplicit bool
	Label       string
	Focus       bool
	Roles       []string
}

type TerminalOpenResult struct {
	Context        Context
	Actions        []TerminalOpenAction
	Manager        TerminalSessionManagerKind
	Initialization *TerminalSessionInitialization
}

type terminalWindowOutcome struct {
	ContainerID          int64
	ManagerStatePossible bool
}

type TerminalSessionManagerKind string

const TerminalSessionManagerHerdr TerminalSessionManagerKind = "herdr"

const defaultTerminalMappingStabilityDelay = time.Second

const terminalRollbackTimeout = 2 * time.Second

var ErrTerminalWindowUnavailable = errors.New("terminal window is unavailable")

func (kind TerminalSessionManagerKind) Validate() error {
	switch kind {
	case TerminalSessionManagerHerdr:
		return nil
	default:
		return fmt.Errorf("unsupported terminal session manager %q; supported values: herdr", kind)
	}
}

// TerminalSessionManager hides the manager-specific process and initialization
// protocol from the registry/Sway terminal lifecycle. Implementations remain a
// compiled allowlist; configuration selects a kind, never an executable or
// command template.
type TerminalSessionManager interface {
	Kind() TerminalSessionManagerKind
	ValidateContext(Context) error
	BuildProcessSpec(Context, string) (ProcessSpec, error)
	ValidateRoles([]string) error
	Initialize(context.Context, Context, []string) (TerminalSessionInitialization, error)
}

type TerminalSessionInitialization struct {
	Manager     TerminalSessionManagerKind `json:"manager"`
	Roles       []string                   `json:"roles"`
	Initialized bool                       `json:"initialized"`
	Reason      string                     `json:"reason,omitempty"`
}

type ContextSwayRequestClient interface {
	RequestContext(context.Context, swayipc.MessageType, []byte) (swayipc.Message, error)
}

// TerminalManager owns the idempotent create-observe-attach-focus transaction
// for one typed Herdr terminal context. Its dependencies are state, process,
// and Sway boundaries rather than command strings.
type TerminalManager struct {
	StateRoot       string
	ProcRoot        string
	Client          ContextSwayRequestClient
	NewContextID    func() (ContextID, error)
	ResolveProgram  func(string) (string, error)
	SessionManager  TerminalSessionManager
	FindPending     func(string, ProcessSpec) ([]int, error)
	FindProcesses   func(string, ContextID) ([]int, error)
	Starter         ProcessStarter
	Now             func() time.Time
	Sleep           func(time.Duration)
	SettleTimeout   time.Duration
	StabilityDelay  time.Duration
	BeforeWindowTxn func()
	RegistryUpdate  func(context.Context, string, func(*Registry) error) (Registry, error)
}

// TerminalAdapterReconfigurer changes an archived identity's closed terminal
// adapter only after proving that neither its marked Sway window nor its exact
// old-adapter launch is still present. The registry lock serializes that proof
// with terminal open, restore, and lifecycle mutations.
type TerminalAdapterReconfigurer struct {
	StateRoot     string
	ProcRoot      string
	Client        ContextSwayRequestClient
	FindProcesses func(string, ContextID) ([]int, error)
}

func (reconfigurer TerminalAdapterReconfigurer) Reconfigure(ctx context.Context, identity TerminalIdentity, adapter TerminalAdapter) (Context, bool, error) {
	if ctx == nil {
		return Context{}, false, errors.New("terminal reconfigure context is nil")
	}
	if !filepath.IsAbs(reconfigurer.StateRoot) || filepath.Clean(reconfigurer.StateRoot) != reconfigurer.StateRoot {
		return Context{}, false, errors.New("terminal state root must be a clean absolute path")
	}
	if reconfigurer.Client == nil {
		return Context{}, false, errors.New("terminal reconfigure dependencies are incomplete")
	}
	if reconfigurer.ProcRoot == "" {
		reconfigurer.ProcRoot = "/proc"
	}
	if reconfigurer.FindProcesses == nil {
		reconfigurer.FindProcesses = FindTerminalAdapterProcesses
	}
	var changed Context
	reconfigured := false
	_, err := UpdateRegistryContext(ctx, reconfigurer.StateRoot, func(registry *Registry) error {
		current, findErr := terminalContextForIdentity(*registry, identity)
		if findErr != nil {
			return findErr
		}
		changed = current
		if current.Launcher.Terminal.Adapter == adapter {
			return nil
		}
		if current.State != ContextArchived {
			return fmt.Errorf("%w: archive context %s first", ErrTerminalAdapterActive, current.ID)
		}
		if err := reconfigurer.requireTerminalClosed(ctx, *registry, current); err != nil {
			return err
		}
		var reconfigureErr error
		changed, reconfigured, reconfigureErr = reconfigureTerminalAdapter(registry, identity, adapter)
		return reconfigureErr
	})
	return changed, reconfigured, err
}

func (reconfigurer TerminalAdapterReconfigurer) requireTerminalClosed(ctx context.Context, registry Registry, current Context) error {
	checkWindow := func() error {
		tree, err := requestTerminalTree(ctx, reconfigurer.Client)
		if err != nil {
			return err
		}
		_, _, exists, err := observedTerminalWindow(tree, registry, current.ID)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: close the window for context %s before reconfiguring", ErrTerminalAdapterInUse, current.ID)
		}
		return nil
	}
	if err := checkWindow(); err != nil {
		return err
	}
	pending, err := reconfigurer.FindProcesses(reconfigurer.ProcRoot, current.ID)
	if err != nil {
		return fmt.Errorf("observe pending terminal launch: %w", err)
	}
	if len(pending) != 0 {
		return fmt.Errorf("%w: wait for old-adapter process(es) %v for context %s to exit", ErrTerminalAdapterInUse, pending, current.ID)
	}
	// Re-observe after the process scan so a launch which became mapped while
	// reading /proc cannot be mistaken for a closed identity.
	return checkWindow()
}

func (manager TerminalManager) Open(ctx context.Context, request TerminalOpenRequest) (TerminalOpenResult, error) {
	if ctx == nil {
		return TerminalOpenResult{}, errors.New("terminal open context is nil")
	}
	if !filepath.IsAbs(manager.StateRoot) || filepath.Clean(manager.StateRoot) != manager.StateRoot {
		return TerminalOpenResult{}, errors.New("terminal state root must be a clean absolute path")
	}
	if manager.Client == nil || manager.NewContextID == nil || manager.ResolveProgram == nil ||
		manager.SessionManager == nil || manager.Starter == nil {
		return TerminalOpenResult{}, errors.New("terminal manager dependencies are incomplete")
	}
	if manager.ProcRoot == "" {
		manager.ProcRoot = "/proc"
	}
	if manager.FindPending == nil {
		manager.FindPending = FindPendingProcessLaunches
	}
	if manager.FindProcesses == nil {
		manager.FindProcesses = FindTerminalAdapterProcesses
	}
	if manager.Now == nil {
		manager.Now = time.Now
	}
	if manager.Sleep == nil {
		manager.Sleep = time.Sleep
	}
	if manager.SettleTimeout <= 0 {
		manager.SettleTimeout = 10 * time.Second
	}
	if manager.StabilityDelay <= 0 {
		manager.StabilityDelay = defaultTerminalMappingStabilityDelay
	}
	if request.New && (request.ContextID != "" || request.Identity != (TerminalIdentity{})) {
		return TerminalOpenResult{}, errors.New("new terminal must not contain a reusable identity")
	}
	if request.ContextID != "" && request.Identity != (TerminalIdentity{}) {
		return TerminalOpenResult{}, errors.New("exact terminal context must not contain a reusable identity")
	}
	if request.ContextID != "" {
		if err := request.ContextID.Validate(); err != nil {
			return TerminalOpenResult{}, fmt.Errorf("invalid terminal context ID: %w", err)
		}
	}
	if len(request.Roles) != 0 {
		if err := manager.SessionManager.ValidateRoles(request.Roles); err != nil {
			return TerminalOpenResult{}, err
		}
	}
	if err := ValidateContextLabel(request.Label); err != nil {
		return TerminalOpenResult{}, err
	}
	if err := ValidateTerminalCwdPath(request.Cwd); err != nil {
		return TerminalOpenResult{}, err
	}
	var result TerminalOpenResult
	err := WithTerminalLifecycleLockContext(ctx, manager.StateRoot, func() error {
		var openErr error
		result, openErr = manager.openSerialized(ctx, request)
		return openErr
	})
	return result, err
}

func (manager TerminalManager) openSerialized(ctx context.Context, request TerminalOpenRequest) (TerminalOpenResult, error) {
	var newID ContextID
	var target Context
	created := false
	updateRegistry := manager.RegistryUpdate
	if updateRegistry == nil {
		updateRegistry = UpdateRegistryContext
	}
	_, err := updateRegistry(ctx, manager.StateRoot, func(registry *Registry) error {
		if request.ContextID != "" {
			index, resolveErr := ResolveContext(*registry, string(request.ContextID))
			if resolveErr != nil {
				return resolveErr
			}
			target = registry.Contexts[index]
			if target.State != ContextActive {
				return fmt.Errorf("%w: activate context %s before opening it", ErrTerminalIdentityArchived, target.ID)
			}
			if target.Launcher.Terminal == nil {
				return errors.New("selected context is not a typed terminal")
			}
			return manager.SessionManager.ValidateContext(target)
		}
		if request.New {
			var createErr error
			target, createErr = CreateTerminalInstanceContext(registry, TerminalInstanceRequest{
				Adapter: request.Adapter,
				Cwd:     request.Cwd,
				Label:   request.Label,
			}, func() (ContextID, error) {
				var idErr error
				newID, idErr = manager.NewContextID()
				return newID, idErr
			})
			created = createErr == nil
			if createErr != nil {
				return createErr
			}
			return manager.SessionManager.ValidateContext(target)
		}
		var ensureErr error
		target, created, ensureErr = EnsureTerminalContext(registry, TerminalContextRequest{
			Identity:    request.Identity,
			Adapter:     request.Adapter,
			Cwd:         request.Cwd,
			CwdExplicit: request.CwdExplicit,
			Label:       request.Label,
		}, func() (ContextID, error) {
			var idErr error
			newID, idErr = manager.NewContextID()
			return newID, idErr
		})
		if ensureErr != nil {
			return ensureErr
		}
		if created {
			return manager.SessionManager.ValidateContext(target)
		}
		return nil
	})
	if err != nil {
		var unknown *statefile.CommitOutcomeUnknownError
		if !errors.As(err, &unknown) {
			return TerminalOpenResult{}, err
		}
		var resolved Context
		var loadErr error
		if request.ContextID != "" {
			resolved, loadErr = terminalContextByID(ctx, manager.StateRoot, request.ContextID)
		} else if request.New {
			resolved, loadErr = terminalContextByID(ctx, manager.StateRoot, newID)
		} else {
			resolved, loadErr = terminalContextByIdentity(ctx, manager.StateRoot, request.Identity)
		}
		if loadErr != nil {
			partial := TerminalOpenResult{}
			if request.New && newID != "" && target.ID == newID && IsTerminalInstanceContext(target) {
				partial.Context = target
				partial.Actions = []TerminalOpenAction{TerminalActionCreated}
			}
			return partial, fmt.Errorf("terminal registry commit outcome unknown: %w; re-observe: %v", err, loadErr)
		}
		target = resolved
		created = newID != "" && target.ID == newID
	}

	result := TerminalOpenResult{Context: target, Manager: manager.SessionManager.Kind()}
	if created {
		result.Actions = append(result.Actions, TerminalActionCreated)
	} else {
		result.Actions = append(result.Actions, TerminalActionReused)
	}
	if manager.BeforeWindowTxn != nil {
		manager.BeforeWindowTxn()
	}
	windowFailed := false
	managerMayHoldState := false
	err = InspectRegistryLockedContext(ctx, manager.StateRoot, func(registry Registry) error {
		index, resolveErr := ResolveContext(registry, string(target.ID))
		if resolveErr != nil {
			return resolveErr
		}
		current := registry.Contexts[index]
		result.Context = current
		if current.Launcher.Kind != LauncherHerdr || current.Launcher.Terminal == nil {
			return errors.New("terminal context launcher changed during open")
		}
		if current.State != ContextActive {
			return fmt.Errorf("%w: activate context %s before opening it", ErrTerminalIdentityArchived, current.ID)
		}
		if request.ContextID != "" {
			if current.ID != request.ContextID || current.Launcher.Terminal == nil {
				return errors.New("selected terminal context changed during open")
			}
		} else if request.New {
			sessionName, nameErr := DeriveTerminalInstanceSessionName(newID)
			if nameErr != nil {
				return nameErr
			}
			if current.ID != newID || !IsTerminalInstanceContext(current) || current.Launcher.Session != sessionName || current.Launcher.Cwd != request.Cwd {
				return errors.New("new terminal context changed during open")
			}
		} else if current.Launcher.Terminal.Identity == nil || *current.Launcher.Terminal.Identity != request.Identity {
			return errors.New("terminal context identity changed during open")
		}
		if request.ContextID == "" && current.Launcher.Terminal.Adapter != request.Adapter {
			return fmt.Errorf("%w: context %s uses %s, config selects %s", ErrTerminalAdapterConflict, current.ID, current.Launcher.Terminal.Adapter, request.Adapter)
		}
		if request.ContextID == "" && request.CwdExplicit && current.Launcher.Cwd != request.Cwd {
			return fmt.Errorf("%w: persisted cwd is %q, requested %q", ErrTerminalIdentityConflict, current.Launcher.Cwd, request.Cwd)
		}
		completedActions := len(result.Actions)
		window, err := manager.ensureWindow(ctx, registry, current, request.Focus, &result)
		managerMayHoldState = window.ManagerStatePossible
		if err != nil {
			windowFailed = true
			return err
		}
		if len(request.Roles) != 0 {
			managerMayHoldState = true
			initialized, initializeErr := manager.SessionManager.Initialize(ctx, current, request.Roles)
			result.Initialization = &initialized
			confirmErr := manager.confirmWindow(ctx, registry, current.ID, window.ContainerID)
			if confirmErr != nil {
				result.Actions = result.Actions[:completedActions]
				windowFailed = true
			}
			return errors.Join(initializeErr, confirmErr)
		}
		return nil
	})
	if err != nil {
		if created {
			if managerMayHoldState {
				if windowFailed {
					return result, fmt.Errorf("%w; context %s remains active because the session manager may hold recoverable state", err, target.ID)
				}
				return result, err
			}
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalRollbackTimeout)
			defer cancel()
			if rollbackErr := manager.rollbackCreatedContext(rollbackCtx, target); rollbackErr == nil {
				return TerminalOpenResult{}, fmt.Errorf("%w; new context %s was rolled back", err, target.ID)
			} else {
				return result, errors.Join(err, fmt.Errorf("roll back new context %s: %w", target.ID, rollbackErr))
			}
		}
		return result, err
	}
	return result, nil
}

func (manager TerminalManager) ensureWindow(ctx context.Context, registry Registry, target Context, focus bool, result *TerminalOpenResult) (terminalWindowOutcome, error) {
	outcome := terminalWindowOutcome{}
	var spec ProcessSpec
	specReady := false
	launched := false
	focusedByManager := false
	deadline := manager.Now().Add(manager.SettleTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		tree, err := manager.requestTree(ctx)
		if err != nil {
			return outcome, err
		}
		window, focused, exists, err := observedTerminalWindow(tree, registry, target.ID)
		if err != nil {
			var issue ManagedWindowIssue
			if errors.As(err, &issue) && issue.ContextID == target.ID {
				outcome.ManagerStatePossible = true
			}
			return outcome, err
		}
		if exists {
			outcome.ManagerStatePossible = true
			if focus && !focused {
				if err := manager.focusWindow(ctx, window.ContainerID, registry, target.ID); err != nil {
					return outcome, err
				}
				focusedByManager = true
			}
			manager.Sleep(manager.StabilityDelay)
			confirmedTree, confirmErr := manager.requestTree(ctx)
			if confirmErr != nil {
				return outcome, confirmErr
			}
			confirmed, _, confirmedExists, confirmErr := observedTerminalWindow(confirmedTree, registry, target.ID)
			if confirmErr != nil {
				return outcome, confirmErr
			}
			if confirmedExists && confirmed.ContainerID == window.ContainerID {
				if launched {
					result.Actions = append(result.Actions, TerminalActionAttached)
				}
				if focus && (launched || focusedByManager) {
					result.Actions = append(result.Actions, TerminalActionFocused)
				} else if !launched {
					result.Actions = append(result.Actions, TerminalActionNoChange)
				}
				outcome.ContainerID = confirmed.ContainerID
				return outcome, nil
			}
			return outcome, fmt.Errorf("%w: terminal container %d did not remain mapped through confirmation", ErrTerminalWindowUnavailable, window.ContainerID)
		}
		if !specReady {
			if err := ValidateTerminalCwd(target.Launcher.Cwd); err != nil {
				return outcome, err
			}
			programName, err := TerminalAdapterExecutableName(target.Launcher.Terminal.Adapter)
			if err != nil {
				return outcome, err
			}
			terminalExecutable, err := manager.ResolveProgram(programName)
			if err != nil {
				return outcome, fmt.Errorf("resolve %s terminal adapter: %w", target.Launcher.Terminal.Adapter, err)
			}
			spec, err = manager.SessionManager.BuildProcessSpec(target, terminalExecutable)
			if err != nil {
				return outcome, err
			}
			specReady = true
		}
		pending, err := manager.FindPending(manager.ProcRoot, spec)
		if err != nil {
			return outcome, fmt.Errorf("observe pending terminal launch: %w", err)
		}
		if len(pending) != 0 {
			outcome.ManagerStatePossible = true
		}
		if len(pending) > 1 {
			return outcome, fmt.Errorf("multiple pending terminal processes %v", pending)
		}
		if len(pending) == 0 {
			if launched {
				return outcome, fmt.Errorf("%w: terminal adapter exited before its window remained mapped", ErrTerminalWindowUnavailable)
			}
			if err := manager.Starter.Start(spec); err != nil {
				return outcome, fmt.Errorf("start terminal adapter: %w", err)
			}
			launched = true
			outcome.ManagerStatePossible = true
		}
		if !manager.Now().Before(deadline) {
			return outcome, fmt.Errorf("%w: terminal window did not appear before the mapping deadline", ErrTerminalWindowUnavailable)
		}
		manager.Sleep(100 * time.Millisecond)
	}
}

func (manager TerminalManager) confirmWindow(ctx context.Context, registry Registry, id ContextID, containerID int64) error {
	manager.Sleep(manager.StabilityDelay)
	tree, err := manager.requestTree(ctx)
	if err != nil {
		return err
	}
	window, _, exists, err := observedTerminalWindow(tree, registry, id)
	if err != nil {
		return err
	}
	if !exists || window.ContainerID != containerID {
		return fmt.Errorf("%w: terminal window disappeared during session initialization", ErrTerminalWindowUnavailable)
	}
	return nil
}

func (manager TerminalManager) rollbackCreatedContext(ctx context.Context, target Context) error {
	_, err := UpdateRegistryContext(ctx, manager.StateRoot, func(registry *Registry) error {
		index, resolveErr := ResolveContext(*registry, string(target.ID))
		if resolveErr != nil {
			return resolveErr
		}
		current := registry.Contexts[index]
		if !reflect.DeepEqual(current, target) {
			return errors.New("new terminal context changed before rollback")
		}
		tree, observeErr := manager.requestTree(ctx)
		if observeErr != nil {
			return fmt.Errorf("re-observe terminal before rollback: %w", observeErr)
		}
		_, _, exists, observeErr := observedTerminalWindow(tree, *registry, target.ID)
		if observeErr != nil {
			return observeErr
		}
		if exists {
			return errors.New("terminal window remapped before rollback")
		}
		pending, observeErr := manager.FindProcesses(manager.ProcRoot, current.ID)
		if observeErr != nil {
			return observeErr
		}
		if len(pending) != 0 {
			return fmt.Errorf("terminal process %v remained pending before rollback", pending)
		}
		_, removeErr := RemoveContext(registry, string(target.ID))
		return removeErr
	})
	if err != nil {
		var unknown *statefile.CommitOutcomeUnknownError
		if errors.As(err, &unknown) {
			if _, loadErr := terminalContextByID(ctx, manager.StateRoot, target.ID); errors.Is(loadErr, ErrContextNotFound) {
				return nil
			}
		}
	}
	return err
}

func (manager TerminalManager) focusWindow(ctx context.Context, containerID int64, registry Registry, id ContextID) error {
	command := fmt.Sprintf("[con_id=%d] focus", containerID)
	message, requestErr := manager.Client.RequestContext(ctx, swayipc.RunCommand, []byte(command))
	if requestErr == nil {
		requestErr = swayipc.CheckRunCommandResponse(message)
	}
	if requestErr == nil {
		return nil
	}
	tree, observeErr := manager.requestTree(ctx)
	if observeErr == nil {
		window, focused, exists, identityErr := observedTerminalWindow(tree, registry, id)
		if identityErr == nil && exists && window.ContainerID == containerID && focused {
			return nil
		}
	}
	return fmt.Errorf("focus terminal context: %w", requestErr)
}

func (manager TerminalManager) requestTree(ctx context.Context) (*swayipc.TreeNode, error) {
	return requestTerminalTree(ctx, manager.Client)
}

func requestTerminalTree(ctx context.Context, client ContextSwayRequestClient) (*swayipc.TreeNode, error) {
	message, err := client.RequestContext(ctx, swayipc.GetTree, nil)
	if err != nil {
		return nil, fmt.Errorf("request Sway tree: %w", err)
	}
	if message.Type != swayipc.GetTree {
		return nil, fmt.Errorf("unexpected Sway tree response type %d", message.Type)
	}
	var root swayipc.TreeNode
	if err := json.Unmarshal(message.Payload, &root); err != nil {
		return nil, fmt.Errorf("decode Sway tree: %w", err)
	}
	return &root, nil
}

func observedTerminalWindow(root *swayipc.TreeNode, registry Registry, id ContextID) (ManagedWindow, bool, bool, error) {
	windows, issues, err := ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		return ManagedWindow{}, false, false, err
	}
	for _, issue := range issues {
		if issue.ContextID == id {
			return ManagedWindow{}, false, false, issue
		}
	}
	window, exists := windows[id]
	if !exists {
		return ManagedWindow{}, false, false, nil
	}
	node := findTerminalTreeNode(root, window.ContainerID)
	if node == nil {
		return ManagedWindow{}, false, false, errors.New("observed terminal container disappeared from Sway tree")
	}
	return window, node.Focused, true, nil
}

func findTerminalTreeNode(node *swayipc.TreeNode, id int64) *swayipc.TreeNode {
	if node == nil {
		return nil
	}
	if node.ID == id {
		return node
	}
	for _, child := range node.Nodes {
		if found := findTerminalTreeNode(child, id); found != nil {
			return found
		}
	}
	for _, child := range node.FloatingNodes {
		if found := findTerminalTreeNode(child, id); found != nil {
			return found
		}
	}
	return nil
}

func terminalContextByIdentity(ctx context.Context, root string, identity TerminalIdentity) (Context, error) {
	registry := Registry{}
	if err := RegistryFile(root).LoadIntoContext(ctx, &registry); err != nil {
		return Context{}, err
	}
	for _, context := range registry.Contexts {
		if context.Launcher.Terminal != nil && context.Launcher.Terminal.Identity != nil &&
			*context.Launcher.Terminal.Identity == identity {
			return context, nil
		}
	}
	return Context{}, ErrContextNotFound
}

func terminalContextByID(ctx context.Context, root string, id ContextID) (Context, error) {
	registry := Registry{}
	if err := RegistryFile(root).LoadIntoContext(ctx, &registry); err != nil {
		return Context{}, err
	}
	index, err := ResolveContext(registry, string(id))
	if err != nil {
		return Context{}, err
	}
	return registry.Contexts[index], nil
}
