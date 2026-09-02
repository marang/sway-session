package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	Identity    TerminalIdentity
	Adapter     TerminalAdapter
	Cwd         string
	CwdExplicit bool
	Label       string
	Focus       bool
}

type TerminalOpenResult struct {
	Context Context
	Actions []TerminalOpenAction
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
	HerdrPaths      func() (HerdrPaths, error)
	ValidateHistory func(HerdrPaths) error
	FindPending     func(string, ProcessSpec) ([]int, error)
	Starter         ProcessStarter
	Now             func() time.Time
	Sleep           func(time.Duration)
	SettleTimeout   time.Duration
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
		manager.HerdrPaths == nil || manager.ValidateHistory == nil || manager.Starter == nil {
		return TerminalOpenResult{}, errors.New("terminal manager dependencies are incomplete")
	}
	if manager.ProcRoot == "" {
		manager.ProcRoot = "/proc"
	}
	if manager.FindPending == nil {
		manager.FindPending = FindPendingProcessLaunches
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
	if request.New && request.Identity != (TerminalIdentity{}) {
		return TerminalOpenResult{}, errors.New("new terminal must not contain a reusable identity")
	}
	if err := ValidateContextLabel(request.Label); err != nil {
		return TerminalOpenResult{}, err
	}
	if err := ValidateTerminalCwdPath(request.Cwd); err != nil {
		return TerminalOpenResult{}, err
	}

	var newID ContextID
	var target Context
	created := false
	updateRegistry := manager.RegistryUpdate
	if updateRegistry == nil {
		updateRegistry = UpdateRegistryContext
	}
	_, err := updateRegistry(ctx, manager.StateRoot, func(registry *Registry) error {
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
			return manager.validateHerdrSessionSocketPaths(target.Launcher.Session)
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
			return manager.validateHerdrSessionSocketPaths(target.Launcher.Session)
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
		if request.New {
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

	result := TerminalOpenResult{Context: target}
	if created {
		result.Actions = append(result.Actions, TerminalActionCreated)
	} else {
		result.Actions = append(result.Actions, TerminalActionReused)
	}
	if manager.BeforeWindowTxn != nil {
		manager.BeforeWindowTxn()
	}
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
		if request.New {
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
		if current.Launcher.Terminal.Adapter != request.Adapter {
			return fmt.Errorf("%w: context %s uses %s, config selects %s", ErrTerminalAdapterConflict, current.ID, current.Launcher.Terminal.Adapter, request.Adapter)
		}
		if request.CwdExplicit && current.Launcher.Cwd != request.Cwd {
			return fmt.Errorf("%w: persisted cwd is %q, requested %q", ErrTerminalIdentityConflict, current.Launcher.Cwd, request.Cwd)
		}
		return manager.ensureWindow(ctx, registry, current, request.Focus, &result)
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func (manager TerminalManager) validateHerdrSessionSocketPaths(sessionName string) error {
	paths, err := manager.HerdrPaths()
	if err != nil {
		return fmt.Errorf("resolve Herdr paths: %w", err)
	}
	return ValidateHerdrSessionSocketPaths(paths.Root, sessionName)
}

func (manager TerminalManager) ensureWindow(ctx context.Context, registry Registry, target Context, focus bool, result *TerminalOpenResult) error {
	tree, err := manager.requestTree(ctx)
	if err != nil {
		return err
	}
	window, focused, exists, err := observedTerminalWindow(tree, registry, target.ID)
	if err != nil {
		return err
	}
	if exists {
		if !focus || focused {
			result.Actions = append(result.Actions, TerminalActionNoChange)
			return nil
		}
		if err := manager.focusWindow(ctx, window.ContainerID, registry, target.ID); err != nil {
			return err
		}
		result.Actions = append(result.Actions, TerminalActionFocused)
		return nil
	}

	if err := ValidateTerminalCwd(target.Launcher.Cwd); err != nil {
		return err
	}
	programName, err := TerminalAdapterExecutableName(target.Launcher.Terminal.Adapter)
	if err != nil {
		return err
	}
	terminalExecutable, err := manager.ResolveProgram(programName)
	if err != nil {
		return fmt.Errorf("resolve %s terminal adapter: %w", target.Launcher.Terminal.Adapter, err)
	}
	herdrExecutable, err := manager.ResolveProgram("herdr")
	if err != nil {
		return fmt.Errorf("resolve Herdr executable: %w", err)
	}
	spec, err := BuildTerminalProcessSpec(target, terminalExecutable, herdrExecutable)
	if err != nil {
		return err
	}
	herdrPaths, err := manager.HerdrPaths()
	if err != nil {
		return fmt.Errorf("resolve Herdr paths: %w", err)
	}
	if err := ValidateHerdrSessionSocketPaths(herdrPaths.Root, target.Launcher.Session); err != nil {
		return err
	}
	pending, err := manager.FindPending(manager.ProcRoot, spec)
	if err != nil {
		return fmt.Errorf("observe pending terminal launch: %w", err)
	}
	if len(pending) > 1 {
		return fmt.Errorf("multiple pending terminal processes %v", pending)
	}
	launched := false
	if len(pending) == 0 {
		if err := manager.ValidateHistory(herdrPaths); err != nil {
			return fmt.Errorf("validate Herdr pane history: %w", err)
		}
		if err := manager.Starter.Start(spec); err != nil {
			return fmt.Errorf("start terminal adapter: %w", err)
		}
		launched = true
		result.Actions = append(result.Actions, TerminalActionAttached)
	}

	deadline := manager.Now().Add(manager.SettleTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tree, err = manager.requestTree(ctx)
		if err != nil {
			return err
		}
		window, focused, exists, err = observedTerminalWindow(tree, registry, target.ID)
		if err != nil {
			return err
		}
		if exists {
			if focus {
				if !focused {
					if err := manager.focusWindow(ctx, window.ContainerID, registry, target.ID); err != nil {
						return err
					}
				}
				if launched || !focused {
					result.Actions = append(result.Actions, TerminalActionFocused)
				} else if !launched {
					result.Actions = append(result.Actions, TerminalActionNoChange)
				}
			}
			return nil
		}
		if !manager.Now().Before(deadline) {
			return errors.New("terminal window did not appear before the mapping deadline")
		}
		manager.Sleep(100 * time.Millisecond)
	}
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
