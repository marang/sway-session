package session

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrTerminalAdapterConflict  = errors.New("terminal adapter conflicts with persisted context")
	ErrTerminalAdapterActive    = errors.New("terminal adapter can change only while the context is archived")
	ErrTerminalAdapterInUse     = errors.New("terminal adapter can change only while no terminal window or launch is present")
	ErrTerminalIdentityConflict = errors.New("terminal identity conflicts with persisted context")
	ErrTerminalIdentityArchived = errors.New("terminal identity is archived")
	ErrTerminalSessionCollision = errors.New("derived terminal session collides with another context")
)

const TerminalContextProvider = "sway-session-terminal"

type TerminalContextRequest struct {
	Identity    TerminalIdentity
	Adapter     TerminalAdapter
	Cwd         string
	CwdExplicit bool
	Label       string
}

// TerminalInstanceRequest describes a fresh persistent terminal which is
// addressed by its context UUID rather than a reusable default/project key.
type TerminalInstanceRequest struct {
	Adapter TerminalAdapter
	Cwd     string
	Label   string
}

func terminalContextForIdentity(registry Registry, identity TerminalIdentity) (Context, error) {
	for _, context := range registry.Contexts {
		if context.Launcher.Kind == LauncherHerdr && context.Launcher.Terminal != nil && context.Launcher.Terminal.Identity != nil &&
			*context.Launcher.Terminal.Identity == identity {
			return context, nil
		}
	}
	return Context{}, fmt.Errorf("%w: terminal identity %s", ErrContextNotFound, identity.String())
}

// ParseTerminalIdentity validates one stable agent-facing project name. It is
// deliberately independent from the project path and any terminal title.
func ParseTerminalIdentity(project string) (TerminalIdentity, error) {
	identity := TerminalIdentity{Kind: TerminalIdentityProject, Project: project}
	terminal := TerminalLauncher{Adapter: TerminalAdapterAlacritty, Identity: &identity}
	if err := terminal.validate(); err != nil {
		return TerminalIdentity{}, err
	}
	return identity, nil
}

// DeriveTerminalSessionName maps a typed terminal identity to a bounded Herdr
// session name. Project names are hashed so user-controlled text is never
// reinterpreted as a Herdr argument or exposed in filesystem names.
func DeriveTerminalSessionName(identity TerminalIdentity) (string, error) {
	terminal := TerminalLauncher{Adapter: TerminalAdapterAlacritty, Identity: &identity}
	if err := terminal.validate(); err != nil {
		return "", err
	}
	switch identity.Kind {
	case TerminalIdentityDefault:
		return "sway-terminal-default", nil
	case TerminalIdentityProject:
		digest := sha256.Sum256([]byte(identity.Project))
		return fmt.Sprintf("sway-terminal-project-%x", digest[:12]), nil
	default:
		return "", fmt.Errorf("unsupported terminal identity kind %q", identity.Kind)
	}
}

// DeriveTerminalInstanceSessionName binds a fresh Herdr session to the same
// UUID that identifies its Sway window and registry context.
func DeriveTerminalInstanceSessionName(id ContextID) (string, error) {
	if err := id.Validate(); err != nil {
		return "", fmt.Errorf("invalid terminal context ID: %w", err)
	}
	return "sway-terminal-" + strings.ReplaceAll(string(id), "-", ""), nil
}

func terminalInstanceSessionMatches(id ContextID, sessionName string) bool {
	current, err := DeriveTerminalInstanceSessionName(id)
	if err != nil {
		return false
	}
	return sessionName == current
}

// IsTerminalInstanceContext recognizes only the complete invariant emitted by
// CreateTerminalInstanceContext. Provider text alone is presentation metadata
// and must not change broker or inventory behavior.
func IsTerminalInstanceContext(context Context) bool {
	if context.Provider != TerminalContextProvider || context.Launcher.Kind != LauncherHerdr ||
		context.Launcher.Terminal == nil || !context.Launcher.Terminal.Instance || context.Launcher.Terminal.Identity != nil {
		return false
	}
	return terminalInstanceSessionMatches(context.ID, context.Launcher.Session)
}

// CreateTerminalInstanceContext always registers a fresh context. Unlike
// EnsureTerminalContext, it has no reusable lookup identity: its generated
// context UUID is the stable agent and Sway identity for the window lifetime.
// Callers serialize this mutation with UpdateRegistry.
func CreateTerminalInstanceContext(registry *Registry, request TerminalInstanceRequest, newContextID func() (ContextID, error)) (Context, error) {
	if registry == nil {
		return Context{}, errors.New("context registry is nil")
	}
	terminal := TerminalLauncher{Adapter: request.Adapter}
	if err := terminal.validate(); err != nil {
		return Context{}, err
	}
	if newContextID == nil {
		return Context{}, errors.New("terminal context ID generator is nil")
	}
	id, err := newContextID()
	if err != nil {
		return Context{}, fmt.Errorf("generate terminal context ID: %w", err)
	}
	sessionName, err := DeriveTerminalInstanceSessionName(id)
	if err != nil {
		return Context{}, err
	}
	label := request.Label
	if label == "" {
		label = "Terminal"
	}
	created := Context{
		ID:       id,
		Label:    label,
		Provider: TerminalContextProvider,
		State:    ContextActive,
		Launcher: Launcher{
			Kind:    LauncherHerdr,
			Session: sessionName,
			Cwd:     request.Cwd,
			Terminal: &TerminalLauncher{
				Adapter:  request.Adapter,
				Instance: true,
			},
		},
	}
	if err := AddContext(registry, created); err != nil {
		return Context{}, err
	}
	return created, nil
}

// EnsureTerminalContext creates at most one context for an exact typed
// identity. Callers serialize this mutation with UpdateRegistry.
func EnsureTerminalContext(registry *Registry, request TerminalContextRequest, newContextID func() (ContextID, error)) (Context, bool, error) {
	if registry == nil {
		return Context{}, false, errors.New("context registry is nil")
	}
	terminal := TerminalLauncher{Adapter: request.Adapter, Identity: &request.Identity}
	if err := terminal.validate(); err != nil {
		return Context{}, false, err
	}
	for _, context := range registry.Contexts {
		if context.Launcher.Kind != LauncherHerdr || context.Launcher.Terminal == nil || context.Launcher.Terminal.Identity == nil {
			continue
		}
		if *context.Launcher.Terminal.Identity != request.Identity {
			continue
		}
		if context.State != ContextActive {
			return Context{}, false, fmt.Errorf("%w: activate context %s before opening it", ErrTerminalIdentityArchived, context.ID)
		}
		if context.Launcher.Terminal.Adapter != request.Adapter {
			return Context{}, false, fmt.Errorf(
				"%w: context %s uses %s, config selects %s",
				ErrTerminalAdapterConflict,
				context.ID,
				context.Launcher.Terminal.Adapter,
				request.Adapter,
			)
		}
		if request.CwdExplicit && context.Launcher.Cwd != request.Cwd {
			return Context{}, false, fmt.Errorf(
				"%w: persisted cwd is %q, requested %q",
				ErrTerminalIdentityConflict,
				context.Launcher.Cwd,
				request.Cwd,
			)
		}
		return context, false, nil
	}
	sessionName, err := DeriveTerminalSessionName(request.Identity)
	if err != nil {
		return Context{}, false, err
	}
	for _, context := range registry.Contexts {
		if context.Launcher.Kind == LauncherHerdr && context.Launcher.Session == sessionName {
			return Context{}, false, fmt.Errorf("%w: %q", ErrTerminalSessionCollision, sessionName)
		}
	}
	if newContextID == nil {
		return Context{}, false, errors.New("terminal context ID generator is nil")
	}
	newID, err := newContextID()
	if err != nil {
		return Context{}, false, fmt.Errorf("generate terminal context ID: %w", err)
	}
	identity := request.Identity
	label := request.Label
	if label == "" {
		if identity.Kind == TerminalIdentityProject {
			label = identity.Project
		} else {
			label = "Terminal"
		}
	}
	created := Context{
		ID:       newID,
		Label:    label,
		Provider: TerminalContextProvider,
		State:    ContextActive,
		Launcher: Launcher{
			Kind:    LauncherHerdr,
			Session: sessionName,
			Cwd:     request.Cwd,
			Terminal: &TerminalLauncher{
				Adapter:  request.Adapter,
				Identity: &identity,
			},
		},
	}
	if err := AddContext(registry, created); err != nil {
		return Context{}, false, err
	}
	return created, true, nil
}

// reconfigureTerminalAdapter changes only the closed adapter discriminator for
// one stable identity. Requiring an archived context prevents automatic restore
// from racing a launcher-policy change; context ID, Herdr session, cwd, and pane
// history remain untouched.
func reconfigureTerminalAdapter(registry *Registry, identity TerminalIdentity, adapter TerminalAdapter) (Context, bool, error) {
	if registry == nil {
		return Context{}, false, errors.New("context registry is nil")
	}
	candidate := TerminalLauncher{Adapter: adapter, Identity: &identity}
	if err := candidate.validate(); err != nil {
		return Context{}, false, err
	}
	for index := range registry.Contexts {
		context := registry.Contexts[index]
		if context.Launcher.Kind == LauncherHerdr && context.Launcher.Terminal != nil && context.Launcher.Terminal.Identity != nil &&
			*context.Launcher.Terminal.Identity == identity {
			if context.Launcher.Terminal.Adapter == adapter {
				return context, false, nil
			}
			if context.State != ContextArchived {
				return Context{}, false, fmt.Errorf("%w: archive context %s first", ErrTerminalAdapterActive, context.ID)
			}
			previous := registry.Contexts[index]
			registry.Contexts[index].Launcher.Terminal.Adapter = adapter
			if err := registry.Validate(); err != nil {
				registry.Contexts[index] = previous
				return Context{}, false, err
			}
			return registry.Contexts[index], true, nil
		}
	}
	return Context{}, false, fmt.Errorf("%w: terminal identity %s", ErrContextNotFound, identity.String())
}
