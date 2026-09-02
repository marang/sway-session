package session

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	ContextsSchemaVersion = 4
	LayoutSchemaVersion   = 1
	MaxContexts           = 128
)

type ContextState string

const (
	ContextActive   ContextState = "active"
	ContextArchived ContextState = "archived"
)

type LauncherKind string

const (
	LauncherHerdr   LauncherKind = "herdr"
	LauncherDesktop LauncherKind = "desktop"
	LauncherFlatpak LauncherKind = "flatpak"
)

type TerminalAdapter string

const (
	TerminalAdapterAlacritty TerminalAdapter = "alacritty"
	TerminalAdapterFoot      TerminalAdapter = "foot"
)

type TerminalIdentityKind string

const (
	TerminalIdentityDefault TerminalIdentityKind = "default"
	TerminalIdentityProject TerminalIdentityKind = "project"
)

type TerminalLauncher struct {
	Adapter  TerminalAdapter   `json:"adapter"`
	Identity *TerminalIdentity `json:"identity,omitempty"`
	Instance bool              `json:"instance,omitempty"`
}

type TerminalIdentity struct {
	Kind    TerminalIdentityKind `json:"kind"`
	Project string               `json:"project,omitempty"`
}

func (identity TerminalIdentity) String() string {
	if identity.Kind == TerminalIdentityProject {
		return string(identity.Kind) + ":" + identity.Project
	}
	return string(identity.Kind)
}

type DesktopEntryOrigin string

const (
	DesktopEntrySystem DesktopEntryOrigin = "system"
	DesktopEntryUser   DesktopEntryOrigin = "user"
)

type FlatpakInstallation string

const (
	FlatpakSystem FlatpakInstallation = "system"
	FlatpakUser   FlatpakInstallation = "user"
)

type WindowProtocol string

const (
	WindowWayland  WindowProtocol = "wayland"
	WindowXWayland WindowProtocol = "xwayland"
)

type ApplicationRestorePolicy string

const (
	ApplicationRestoreFollow ApplicationRestorePolicy = "follow"
	ApplicationRestorePinned ApplicationRestorePolicy = "pinned"
)

// Registry is the versioned contexts.json document written by sway-session.
type Registry struct {
	Version     int                 `json:"version"`
	Preferences RegistryPreferences `json:"preferences"`
	Contexts    []Context           `json:"contexts"`
}

type RegistryPreferences struct {
	DesktopIndicators bool `json:"desktop_indicators"`
}

type Context struct {
	ID         ContextID    `json:"id"`
	Label      string       `json:"label,omitempty"`
	Provider   string       `json:"provider,omitempty"`
	State      ContextState `json:"state"`
	ArchivedAt *time.Time   `json:"archived_at,omitempty"`
	Launcher   Launcher     `json:"launcher"`
	App        *Application `json:"app,omitempty"`
}

// Launcher is a validated tagged union. Fields for launcher kinds other than
// Kind must remain empty, so persistent state never turns into a generic
// command or argument store.
type Launcher struct {
	Kind LauncherKind `json:"kind"`

	Session  string            `json:"session,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Terminal *TerminalLauncher `json:"terminal,omitempty"`

	DesktopID                string             `json:"desktop_id,omitempty"`
	DesktopOrigin            DesktopEntryOrigin `json:"desktop_origin,omitempty"`
	DesktopPath              string             `json:"desktop_path,omitempty"`
	DesktopEntrySHA256       string             `json:"desktop_entry_sha256,omitempty"`
	ApprovedDesktopPath      string             `json:"approved_desktop_path,omitempty"`
	ApprovedExecutablePath   string             `json:"approved_executable_path,omitempty"`
	ApprovedExecutableSHA256 string             `json:"approved_executable_sha256,omitempty"`

	FlatpakID           string              `json:"flatpak_id,omitempty"`
	FlatpakInstallation FlatpakInstallation `json:"flatpak_installation,omitempty"`
}

type Application struct {
	Identity      ApplicationIdentity      `json:"identity"`
	DesiredOpen   bool                     `json:"desired_open"`
	RestorePolicy ApplicationRestorePolicy `json:"restore_policy"`
}

// ApplicationIdentity contains only stable compositor-visible identifiers and
// desktop metadata. It deliberately excludes titles, process arguments, URLs,
// profiles, and application-private state.
type ApplicationIdentity struct {
	Protocol       WindowProtocol `json:"protocol"`
	WaylandAppID   string         `json:"wayland_app_id,omitempty"`
	X11Class       string         `json:"x11_class,omitempty"`
	X11Instance    string         `json:"x11_instance,omitempty"`
	StartupWMClass string         `json:"startup_wm_class,omitempty"`
	SandboxAppID   string         `json:"sandbox_app_id,omitempty"`
}

// LayoutSnapshot is the versioned layout.json document written by the daemon.
type LayoutSnapshot struct {
	Version    int               `json:"version"`
	Workspaces []WorkspaceLayout `json:"workspaces"`
}

type WorkspaceLayout struct {
	Name              string               `json:"name"`
	RestoreMode       WorkspaceRestoreMode `json:"restore_mode"`
	PlacementContexts []ContextID          `json:"placement_contexts,omitempty"`
	Tiling            *LayoutNode          `json:"tiling,omitempty"`
	Floating          []LayoutNode         `json:"floating,omitempty"`
	FocusedContext    *ContextID           `json:"focused_context,omitempty"`
}

type WorkspaceRestoreMode string

const (
	WorkspaceRestoreLayout        WorkspaceRestoreMode = "layout"
	WorkspaceRestorePlacementOnly WorkspaceRestoreMode = "placement_only"
)

type LayoutKind string

const (
	LayoutSplitHorizontal LayoutKind = "splith"
	LayoutSplitVertical   LayoutKind = "splitv"
	LayoutTabbed          LayoutKind = "tabbed"
	LayoutStacked         LayoutKind = "stacked"
)

type FullscreenMode string

const (
	FullscreenNone      FullscreenMode = ""
	FullscreenWorkspace FullscreenMode = "workspace"
	FullscreenGlobal    FullscreenMode = "global"
)

// LayoutNode is either a managed leaf (ContextID) or a layout parent
// (Layout and Children). Slice order is the saved Sway child order. Proportion
// is a tiled node's parent-relative share in the range [0, 1]; zero means that
// no proportion was captured. Each top-level floating node uses Geometry
// instead; a floating layout parent's descendants retain tiled proportions.
type LayoutNode struct {
	ContextID  *ContextID     `json:"context_id,omitempty"`
	Layout     LayoutKind     `json:"layout,omitempty"`
	Children   []LayoutNode   `json:"children,omitempty"`
	Proportion float64        `json:"proportion,omitempty"`
	Fullscreen FullscreenMode `json:"fullscreen,omitempty"`
	Geometry   *Geometry      `json:"geometry,omitempty"`
}

type Geometry struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// UnsupportedVersionError identifies state that requires a different schema
// reader instead of being partially interpreted.
type UnsupportedVersionError struct {
	Document string
	Got      int
	Want     int
}

func (err *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("unsupported %s schema version %d; expected %d", err.Document, err.Got, err.Want)
}

func (registry *Registry) Validate() error {
	if registry == nil {
		return errors.New("context registry is nil")
	}
	if err := validateVersion("context registry", registry.Version, ContextsSchemaVersion); err != nil {
		return err
	}
	if registry.Contexts == nil {
		return errors.New("context registry must contain a contexts array")
	}
	if len(registry.Contexts) > MaxContexts {
		return fmt.Errorf("context registry contains %d contexts; maximum is %d", len(registry.Contexts), MaxContexts)
	}
	seen := make(map[ContextID]struct{}, len(registry.Contexts))
	seenLaunchers := make(map[launcherIdentity]int, len(registry.Contexts))
	seenTerminals := make(map[terminalIdentity]int, len(registry.Contexts))
	seenApplications := make([]applicationIdentityRecord, 0, len(registry.Contexts))
	for index := range registry.Contexts {
		context := &registry.Contexts[index]
		if err := context.validate(); err != nil {
			return fmt.Errorf("contexts[%d]: %w", index, err)
		}
		if _, exists := seen[context.ID]; exists {
			return fmt.Errorf("contexts[%d]: duplicate context ID %q", index, context.ID)
		}
		seen[context.ID] = struct{}{}
		identity := context.Launcher.identity()
		if previous, exists := seenLaunchers[identity]; exists {
			return fmt.Errorf(
				"contexts[%d]: launcher %q identity %q is already used by contexts[%d]",
				index,
				identity.kind,
				identity.value,
				previous,
			)
		}
		seenLaunchers[identity] = index
		if identity, ok := context.Launcher.terminalIdentity(); ok {
			if previous, exists := seenTerminals[identity]; exists {
				return fmt.Errorf(
					"contexts[%d]: terminal identity %q is already used by contexts[%d]",
					index,
					identity.String(),
					previous,
				)
			}
			seenTerminals[identity] = index
		}
		if context.App != nil {
			for _, previous := range seenApplications {
				if applicationIdentitiesOverlap(context.App.Identity, previous.identity) {
					return fmt.Errorf("contexts[%d]: application identity overlaps contexts[%d]", index, previous.index)
				}
			}
			seenApplications = append(seenApplications, applicationIdentityRecord{identity: context.App.Identity, index: index})
		}
	}
	return nil
}

type launcherIdentity struct {
	kind  LauncherKind
	value string
}

type terminalIdentity struct {
	kind    TerminalIdentityKind
	project string
}

func (identity terminalIdentity) String() string {
	if identity.kind == TerminalIdentityProject {
		return string(identity.kind) + ":" + identity.project
	}
	return string(identity.kind)
}

type applicationIdentityRecord struct {
	identity ApplicationIdentity
	index    int
}

func applicationIdentitiesOverlap(left ApplicationIdentity, right ApplicationIdentity) bool {
	if left.Protocol != right.Protocol {
		return false
	}
	primaryMatches := left.WaylandAppID == right.WaylandAppID
	if left.Protocol == WindowXWayland {
		primaryMatches = left.X11Class == right.X11Class && left.X11Instance == right.X11Instance
	}
	if !primaryMatches {
		return false
	}
	return left.SandboxAppID == "" || right.SandboxAppID == "" || left.SandboxAppID == right.SandboxAppID
}

func (launcher Launcher) identity() launcherIdentity {
	switch launcher.Kind {
	case LauncherHerdr:
		return launcherIdentity{kind: launcher.Kind, value: launcher.Session}
	case LauncherDesktop:
		return launcherIdentity{kind: launcher.Kind, value: launcher.DesktopID}
	case LauncherFlatpak:
		return launcherIdentity{kind: launcher.Kind, value: launcher.FlatpakID}
	default:
		return launcherIdentity{kind: launcher.Kind}
	}
}

func (launcher Launcher) terminalIdentity() (terminalIdentity, bool) {
	if launcher.Terminal == nil || launcher.Terminal.Identity == nil {
		return terminalIdentity{}, false
	}
	return terminalIdentity{
		kind:    launcher.Terminal.Identity.Kind,
		project: launcher.Terminal.Identity.Project,
	}, true
}

func (context *Context) validate() error {
	if err := context.ID.Validate(); err != nil {
		return fmt.Errorf("invalid ID: %w", err)
	}
	if err := ValidateContextLabel(context.Label); err != nil {
		return err
	}
	if err := validateMetadata("provider", context.Provider); err != nil {
		return err
	}
	if context.State != ContextActive && context.State != ContextArchived {
		return fmt.Errorf("invalid state %q", context.State)
	}
	if context.ArchivedAt != nil {
		if context.State != ContextArchived {
			return errors.New("only archived contexts may contain archived_at")
		}
		if context.ArchivedAt.IsZero() || context.ArchivedAt.Location() != time.UTC {
			return errors.New("archived_at must be a non-zero canonical UTC timestamp")
		}
	}
	if err := context.Launcher.validate(); err != nil {
		return err
	}
	switch context.Launcher.Kind {
	case LauncherHerdr:
		if context.App != nil {
			return errors.New("herdr context must not contain desktop application state")
		}
		if context.Launcher.Terminal.Instance {
			if context.Provider != TerminalContextProvider {
				return errors.New("terminal instance must use the reserved provider")
			}
			sessionName, err := DeriveTerminalInstanceSessionName(context.ID)
			if err != nil || context.Launcher.Session != sessionName {
				return errors.New("terminal instance session must be derived from its context ID")
			}
		}
	case LauncherDesktop, LauncherFlatpak:
		if context.App == nil {
			return errors.New("desktop application launcher requires application state")
		}
		if err := context.App.validate(); err != nil {
			return fmt.Errorf("invalid application state: %w", err)
		}
		if context.Launcher.Kind == LauncherDesktop && context.App.Identity.SandboxAppID != "" {
			return errors.New("desktop launcher must not contain a Flatpak sandbox identity")
		}
		if context.Launcher.Kind == LauncherFlatpak {
			if context.App.Identity.SandboxAppID != context.Launcher.FlatpakID {
				return errors.New("flatpak window sandbox identity must match launcher application ID")
			}
			if !validFlatpakID(context.App.Identity.SandboxAppID) {
				return errors.New("flatpak window sandbox identity must be a valid Flatpak application ID")
			}
		}
	}
	return nil
}

// Validate checks one context independently of a registry.
func (context *Context) Validate() error {
	if context == nil {
		return errors.New("context is nil")
	}
	return context.validate()
}

func (launcher *Launcher) validate() error {
	switch launcher.Kind {
	case LauncherHerdr:
		return launcher.validateHerdr()
	case LauncherDesktop:
		return launcher.validateDesktop()
	case LauncherFlatpak:
		return launcher.validateFlatpak()
	default:
		return fmt.Errorf("unsupported launcher kind %q", launcher.Kind)
	}
}

func (launcher *Launcher) validateHerdr() error {
	if launcher.Session == "default" {
		return errors.New("launcher session name \"default\" is reserved by Herdr and cannot be purged safely")
	}
	if !validSessionName(launcher.Session) {
		return errors.New("launcher session must start with an ASCII letter or digit and contain at most 64 ASCII letters, digits, dots, underscores, or hyphens")
	}
	if launcher.Cwd == "" || !filepath.IsAbs(launcher.Cwd) {
		return errors.New("launcher cwd must be an absolute path")
	}
	if filepath.Clean(launcher.Cwd) != launcher.Cwd {
		return errors.New("launcher cwd must be a clean absolute path")
	}
	if containsControl(launcher.Cwd) {
		return errors.New("launcher cwd must not contain control characters")
	}
	if launcher.hasDesktopFields() || launcher.hasFlatpakFields() {
		return errors.New("herdr launcher must not contain desktop or Flatpak fields")
	}
	if launcher.Terminal == nil {
		return errors.New("herdr launcher requires terminal configuration")
	}
	if err := launcher.Terminal.validate(); err != nil {
		return fmt.Errorf("invalid terminal configuration: %w", err)
	}
	return nil
}

func (launcher *Launcher) validateDesktop() error {
	if launcher.Session != "" || launcher.Cwd != "" || launcher.Terminal != nil || launcher.hasFlatpakFields() {
		return errors.New("desktop launcher must not contain Herdr or Flatpak fields")
	}
	if err := validateDesktopID(launcher.DesktopID); err != nil {
		return fmt.Errorf("invalid desktop ID: %w", err)
	}
	if err := validateAbsoluteFilePath("desktop path", launcher.DesktopPath, ".desktop"); err != nil {
		return err
	}
	switch launcher.DesktopOrigin {
	case DesktopEntrySystem:
		if launcher.DesktopEntrySHA256 != "" || launcher.ApprovedDesktopPath != "" || launcher.ApprovedExecutablePath != "" || launcher.ApprovedExecutableSHA256 != "" {
			return errors.New("system desktop launcher must not contain user approval fields")
		}
	case DesktopEntryUser:
		if err := validateSHA256("desktop entry", launcher.DesktopEntrySHA256); err != nil {
			return err
		}
		if err := validateAbsoluteFilePath("approved desktop path", launcher.ApprovedDesktopPath, ".desktop"); err != nil {
			return err
		}
		if (launcher.ApprovedExecutablePath == "") != (launcher.ApprovedExecutableSHA256 == "") {
			return errors.New("approved executable path and checksum must either both be present or both be absent")
		}
		if launcher.ApprovedExecutablePath != "" {
			if err := validateAbsoluteFilePath("approved executable path", launcher.ApprovedExecutablePath, ""); err != nil {
				return err
			}
			if err := validateSHA256("approved executable", launcher.ApprovedExecutableSHA256); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported desktop entry origin %q", launcher.DesktopOrigin)
	}
	return nil
}

func (launcher *Launcher) validateFlatpak() error {
	if launcher.Session != "" || launcher.Cwd != "" || launcher.Terminal != nil || launcher.hasDesktopFields() {
		return errors.New("flatpak launcher must not contain Herdr or desktop fields")
	}
	if !validFlatpakID(launcher.FlatpakID) {
		return errors.New("flatpak application ID must be a valid reverse-DNS D-Bus name")
	}
	switch launcher.FlatpakInstallation {
	case FlatpakSystem, FlatpakUser:
	default:
		return fmt.Errorf("unsupported Flatpak installation %q", launcher.FlatpakInstallation)
	}
	return nil
}

func (terminal *TerminalLauncher) validate() error {
	if terminal == nil {
		return errors.New("terminal configuration is nil")
	}
	switch terminal.Adapter {
	case TerminalAdapterAlacritty, TerminalAdapterFoot:
	default:
		return fmt.Errorf("unsupported terminal adapter %q", terminal.Adapter)
	}
	if terminal.Identity == nil {
		return nil
	}
	if terminal.Instance {
		return errors.New("terminal instance must not contain a reusable identity")
	}
	switch terminal.Identity.Kind {
	case TerminalIdentityDefault:
		if terminal.Identity.Project != "" {
			return errors.New("default terminal identity must not contain a project name")
		}
	case TerminalIdentityProject:
		if !validSessionName(terminal.Identity.Project) {
			return errors.New("terminal project name must start with an ASCII letter or digit and contain at most 64 ASCII letters, digits, dots, underscores, or hyphens")
		}
	default:
		return fmt.Errorf("unsupported terminal identity kind %q", terminal.Identity.Kind)
	}
	return nil
}

func (launcher *Launcher) hasDesktopFields() bool {
	return launcher.DesktopID != "" || launcher.DesktopOrigin != "" || launcher.DesktopPath != "" ||
		launcher.DesktopEntrySHA256 != "" || launcher.ApprovedDesktopPath != "" || launcher.ApprovedExecutablePath != "" || launcher.ApprovedExecutableSHA256 != ""
}

func (launcher *Launcher) hasFlatpakFields() bool {
	return launcher.FlatpakID != "" || launcher.FlatpakInstallation != ""
}

func (application *Application) validate() error {
	if application == nil {
		return errors.New("application state is nil")
	}
	if err := application.Identity.validate(); err != nil {
		return err
	}
	switch application.RestorePolicy {
	case ApplicationRestoreFollow, ApplicationRestorePinned:
	default:
		return fmt.Errorf("unsupported restore policy %q", application.RestorePolicy)
	}
	if application.RestorePolicy == ApplicationRestorePinned && !application.DesiredOpen {
		return errors.New("pinned application must remain desired-open")
	}
	return nil
}

func (identity *ApplicationIdentity) validate() error {
	if identity == nil {
		return errors.New("application identity is nil")
	}
	for name, value := range map[string]string{
		"Wayland app ID": identity.WaylandAppID,
		"X11 class":      identity.X11Class,
		"X11 instance":   identity.X11Instance,
		"StartupWMClass": identity.StartupWMClass,
		"sandbox app ID": identity.SandboxAppID,
	} {
		if err := validateIdentityValue(name, value); err != nil {
			return err
		}
	}
	switch identity.Protocol {
	case WindowWayland:
		if identity.WaylandAppID == "" {
			return errors.New("wayland identity requires an app ID")
		}
		if identity.X11Class != "" || identity.X11Instance != "" {
			return errors.New("wayland identity must not contain X11 class or instance")
		}
	case WindowXWayland:
		if identity.WaylandAppID != "" {
			return errors.New("XWayland identity must not contain a Wayland app ID")
		}
		if identity.X11Class == "" || identity.X11Instance == "" {
			return errors.New("XWayland identity requires both class and instance")
		}
	default:
		return fmt.Errorf("unsupported window protocol %q", identity.Protocol)
	}
	if identity.SandboxAppID != "" && !validBusName(identity.SandboxAppID) {
		return errors.New("sandbox app ID must be a valid D-Bus name")
	}
	return nil
}

func (snapshot *LayoutSnapshot) Validate() error {
	if snapshot == nil {
		return errors.New("layout snapshot is nil")
	}
	if err := validateVersion("layout snapshot", snapshot.Version, LayoutSchemaVersion); err != nil {
		return err
	}
	if snapshot.Workspaces == nil {
		return errors.New("layout snapshot must contain a workspaces array")
	}
	workspaceNames := make(map[string]struct{}, len(snapshot.Workspaces))
	contextIDs := make(map[ContextID]string)
	globalFullscreen := false
	for index := range snapshot.Workspaces {
		workspace := &snapshot.Workspaces[index]
		if err := workspace.validate(contextIDs, &globalFullscreen); err != nil {
			return fmt.Errorf("workspaces[%d]: %w", index, err)
		}
		if _, exists := workspaceNames[workspace.Name]; exists {
			return fmt.Errorf("workspaces[%d]: duplicate workspace %q", index, workspace.Name)
		}
		workspaceNames[workspace.Name] = struct{}{}
	}
	return nil
}

func (workspace *WorkspaceLayout) validate(contextIDs map[ContextID]string, globalFullscreen *bool) error {
	if strings.TrimSpace(workspace.Name) == "" || workspace.Name != strings.TrimSpace(workspace.Name) || containsControl(workspace.Name) {
		return errors.New("workspace name must be non-empty, trimmed, and contain no control characters")
	}
	if len(workspace.Name) > 256 {
		return errors.New("workspace name must be at most 256 characters")
	}
	if workspace.Name == RestoreStagingWorkspace {
		return fmt.Errorf("workspace name %q is reserved for restore staging", workspace.Name)
	}
	localIDs := make(map[ContextID]struct{})
	fullscreen := fullscreenValidation{global: globalFullscreen}
	switch workspace.RestoreMode {
	case WorkspaceRestoreLayout:
		if len(workspace.PlacementContexts) != 0 {
			return errors.New("layout restore must not contain placement-only contexts")
		}
		if workspace.Tiling == nil && len(workspace.Floating) == 0 {
			return errors.New("workspace must contain at least one managed window")
		}
		if workspace.Tiling != nil {
			if err := workspace.Tiling.validate(false, localIDs, &fullscreen); err != nil {
				return fmt.Errorf("tiling: %w", err)
			}
		}
		for index := range workspace.Floating {
			if err := workspace.Floating[index].validate(true, localIDs, &fullscreen); err != nil {
				return fmt.Errorf("floating[%d]: %w", index, err)
			}
		}
	case WorkspaceRestorePlacementOnly:
		if workspace.Tiling != nil || len(workspace.Floating) != 0 || workspace.FocusedContext != nil {
			return errors.New("placement-only restore must not contain layout, floating, or focus state")
		}
		if len(workspace.PlacementContexts) == 0 {
			return errors.New("placement-only restore must contain at least one managed context")
		}
		for index, id := range workspace.PlacementContexts {
			if err := id.Validate(); err != nil {
				return fmt.Errorf("placement_contexts[%d]: %w", index, err)
			}
			if _, exists := localIDs[id]; exists {
				return fmt.Errorf("placement_contexts[%d]: duplicate context %q", index, id)
			}
			localIDs[id] = struct{}{}
		}
	default:
		return fmt.Errorf("unsupported workspace restore mode %q", workspace.RestoreMode)
	}
	for id := range localIDs {
		if otherWorkspace, exists := contextIDs[id]; exists {
			return fmt.Errorf("context %q also appears in workspace %q", id, otherWorkspace)
		}
		contextIDs[id] = workspace.Name
	}
	if workspace.FocusedContext != nil {
		if err := workspace.FocusedContext.Validate(); err != nil {
			return fmt.Errorf("invalid focused context: %w", err)
		}
		if _, exists := localIDs[*workspace.FocusedContext]; !exists {
			return errors.New("focused context is not present in the workspace")
		}
	}
	return nil
}

type fullscreenValidation struct {
	workspace bool
	global    *bool
}

func (state *fullscreenValidation) record(mode FullscreenMode) error {
	switch mode {
	case FullscreenNone:
		return nil
	case FullscreenWorkspace:
		if state.workspace {
			return errors.New("workspace contains multiple fullscreen nodes")
		}
		state.workspace = true
	case FullscreenGlobal:
		if state.workspace {
			return errors.New("workspace contains multiple fullscreen nodes")
		}
		if *state.global {
			return errors.New("snapshot contains multiple global fullscreen nodes")
		}
		state.workspace = true
		*state.global = true
	}
	return nil
}

func (node *LayoutNode) validate(floatingRoot bool, seen map[ContextID]struct{}, fullscreen *fullscreenValidation) error {
	if math.IsNaN(node.Proportion) || math.IsInf(node.Proportion, 0) || node.Proportion < 0 || node.Proportion > 1 {
		return errors.New("proportion must be finite and between zero and one")
	}
	if floatingRoot && node.Proportion != 0 {
		return errors.New("floating entries must not store a tiling proportion")
	}
	if !validFullscreen(node.Fullscreen) {
		return fmt.Errorf("unsupported fullscreen mode %q", node.Fullscreen)
	}
	if err := fullscreen.record(node.Fullscreen); err != nil {
		return err
	}
	isLeaf := node.ContextID != nil
	if isLeaf {
		if node.Layout != "" || len(node.Children) != 0 {
			return errors.New("managed leaf must not contain a layout or children")
		}
		if err := node.ContextID.Validate(); err != nil {
			return fmt.Errorf("invalid context ID: %w", err)
		}
		if _, exists := seen[*node.ContextID]; exists {
			return fmt.Errorf("duplicate context %q", *node.ContextID)
		}
		seen[*node.ContextID] = struct{}{}
		if floatingRoot && node.Geometry == nil {
			return errors.New("floating entries must store geometry")
		}
		if node.Geometry != nil {
			if !floatingRoot {
				return errors.New("only top-level floating entries may store geometry")
			}
			if err := node.Geometry.validate(); err != nil {
				return err
			}
		}
		return nil
	}
	if !validLayout(node.Layout) {
		return fmt.Errorf("unsupported layout %q", node.Layout)
	}
	if len(node.Children) == 0 {
		return errors.New("layout parent must contain children")
	}
	if floatingRoot && node.Geometry == nil {
		return errors.New("floating entries must store geometry")
	}
	if node.Geometry != nil {
		if !floatingRoot {
			return errors.New("only top-level floating entries may store geometry")
		}
		if err := node.Geometry.validate(); err != nil {
			return err
		}
	}
	for index := range node.Children {
		if err := node.Children[index].validate(false, seen, fullscreen); err != nil {
			return fmt.Errorf("children[%d]: %w", index, err)
		}
	}
	return nil
}

func (geometry *Geometry) validate() error {
	if geometry.Width <= 0 || geometry.Height <= 0 {
		return errors.New("floating geometry width and height must be positive")
	}
	return nil
}

func validateVersion(document string, version int, expected int) error {
	if version != expected {
		return &UnsupportedVersionError{Document: document, Got: version, Want: expected}
	}
	return nil
}

func validateMetadata(name string, value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not have surrounding whitespace", name)
	}
	if len(value) > 256 {
		return fmt.Errorf("%s must be at most 256 characters", name)
	}
	if containsControl(value) {
		return fmt.Errorf("%s must not contain control characters", name)
	}
	return nil
}

// ValidateContextLabel checks optional presentation metadata stored on a
// persistent context. Callers that accept a label must validate it before
// initiating state migration or other side effects.
func ValidateContextLabel(value string) error {
	return validateMetadata("label", value)
}

func validateDesktopID(value string) error {
	if value == "" || value != strings.TrimSpace(value) || containsControl(value) {
		return errors.New("desktop ID must be non-empty, trimmed, and contain no control characters")
	}
	if len(value) > 512 {
		return errors.New("desktop ID must be at most 512 bytes")
	}
	if strings.ContainsAny(value, `/\\`) || !strings.HasSuffix(value, ".desktop") || value == ".desktop" {
		return errors.New("desktop ID must be one path-free name ending in .desktop")
	}
	return nil
}

func validateAbsoluteFilePath(name string, value string, suffix string) error {
	if value == "" || !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path", name)
	}
	if filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}
	if len(value) > 4096 || containsControl(value) {
		return fmt.Errorf("%s must be at most 4096 bytes and contain no control characters", name)
	}
	if suffix != "" && !strings.HasSuffix(value, suffix) {
		return fmt.Errorf("%s must end in %s", name, suffix)
	}
	return nil
}

func validateSHA256(name string, value string) error {
	if len(value) != 64 || value != strings.ToLower(value) {
		return fmt.Errorf("%s SHA-256 must contain 64 lowercase hexadecimal characters", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("%s SHA-256 must contain 64 lowercase hexadecimal characters", name)
	}
	return nil
}

func validateIdentityValue(name string, value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) || containsControl(value) {
		return fmt.Errorf("%s must be trimmed and contain no control characters", name)
	}
	if len(value) > 256 {
		return fmt.Errorf("%s must be at most 256 bytes", name)
	}
	return nil
}

// validBusName implements the conservative common subset used by D-Bus
// well-known names and Flatpak application IDs. Flatpak itself remains the
// authority for whether an installed ID can be launched.
func validBusName(value string) bool {
	if len(value) == 0 || len(value) > 255 || !strings.Contains(value, ".") {
		return false
	}
	for _, component := range strings.Split(value, ".") {
		if component == "" || component[0] >= '0' && component[0] <= '9' {
			return false
		}
		for _, character := range component {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' || character == '_' || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// validFlatpakID applies Flatpak's stricter application-ID conventions on top
// of the D-Bus shape: at least three components, lowercase domain components,
// and hyphens only in the final application component.
func validFlatpakID(value string) bool {
	if !validBusName(value) {
		return false
	}
	components := strings.Split(value, ".")
	if len(components) < 3 {
		return false
	}
	for _, component := range components[:len(components)-1] {
		for _, character := range component {
			if character >= 'A' && character <= 'Z' || character == '-' {
				return false
			}
		}
	}
	return true
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func validSessionName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	first := value[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validLayout(layout LayoutKind) bool {
	switch layout {
	case LayoutSplitHorizontal, LayoutSplitVertical, LayoutTabbed, LayoutStacked:
		return true
	default:
		return false
	}
}

func validFullscreen(mode FullscreenMode) bool {
	switch mode {
	case FullscreenNone, FullscreenWorkspace, FullscreenGlobal:
		return true
	default:
		return false
	}
}
