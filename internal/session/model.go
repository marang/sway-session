package session

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	ContextsSchemaVersion = 1
	LayoutSchemaVersion   = 1
	MaxContexts           = 128
)

type ContextState string

const (
	ContextActive   ContextState = "active"
	ContextArchived ContextState = "archived"
)

type LauncherKind string

const LauncherHerdr LauncherKind = "herdr"

// Registry is the versioned contexts.json document written by sway-session.
type Registry struct {
	Version  int       `json:"version"`
	Contexts []Context `json:"contexts"`
}

type Context struct {
	ID       ContextID    `json:"id"`
	Label    string       `json:"label,omitempty"`
	Provider string       `json:"provider,omitempty"`
	State    ContextState `json:"state"`
	Launcher Launcher     `json:"launcher"`
}

type Launcher struct {
	Kind    LauncherKind `json:"kind"`
	Session string       `json:"session"`
	Cwd     string       `json:"cwd"`
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
				"contexts[%d]: launcher %q session %q is already used by contexts[%d]",
				index,
				identity.kind,
				identity.session,
				previous,
			)
		}
		seenLaunchers[identity] = index
	}
	return nil
}

type launcherIdentity struct {
	kind    LauncherKind
	session string
}

func (launcher Launcher) identity() launcherIdentity {
	return launcherIdentity{kind: launcher.Kind, session: launcher.Session}
}

func (context *Context) validate() error {
	if err := context.ID.Validate(); err != nil {
		return fmt.Errorf("invalid ID: %w", err)
	}
	if err := validateMetadata("label", context.Label); err != nil {
		return err
	}
	if err := validateMetadata("provider", context.Provider); err != nil {
		return err
	}
	if context.State != ContextActive && context.State != ContextArchived {
		return fmt.Errorf("invalid state %q", context.State)
	}
	return context.Launcher.validate()
}

// Validate checks one context independently of a registry.
func (context *Context) Validate() error {
	if context == nil {
		return errors.New("context is nil")
	}
	return context.validate()
}

func (launcher *Launcher) validate() error {
	if launcher.Kind != LauncherHerdr {
		return fmt.Errorf("unsupported launcher kind %q", launcher.Kind)
	}
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
