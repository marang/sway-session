package session

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/marang/sway-session/internal/swayipc"
)

// WindowApplication is the bounded compositor evidence used while registering
// one already-running desktop application. Titles, process arguments, and
// application-private state are intentionally absent.
type WindowApplication struct {
	ContainerID  int64
	Workspace    string
	Identity     ApplicationIdentity
	ContextMarks []ContextID
}

// ApplicationResolution contains an exact focused/window identity and every
// desktop entry which can explain it. Callers must never guess when Candidates
// contains more than one entry.
type ApplicationResolution struct {
	Window     WindowApplication
	Candidates []DesktopEntry
	Registered *Context
}

// ResolveFocusedApplication resolves the focused eligible top-level window.
func ResolveFocusedApplication(root *swayipc.TreeNode, catalog DesktopCatalog, registry Registry) (ApplicationResolution, error) {
	window, err := FocusedApplicationWindow(root)
	if err != nil {
		return ApplicationResolution{}, err
	}
	return ResolveApplication(window, catalog, registry)
}

// ResolveApplication finds an existing registration and exact desktop-entry
// candidates for one compositor window.
func ResolveApplication(window WindowApplication, catalog DesktopCatalog, registry Registry) (ApplicationResolution, error) {
	if err := registry.Validate(); err != nil {
		return ApplicationResolution{}, fmt.Errorf("validate context registry: %w", err)
	}
	if window.ContainerID <= 0 || !validApplicationWorkspace(window.Workspace) {
		return ApplicationResolution{}, errors.New("application window must be on a normal Sway workspace")
	}
	if err := window.Identity.validate(); err != nil {
		return ApplicationResolution{}, fmt.Errorf("invalid application window identity: %w", err)
	}
	result := ApplicationResolution{Window: window, Candidates: DesktopCandidatesForWindow(window, catalog)}
	if len(window.ContextMarks) > 1 {
		return ApplicationResolution{}, errors.New("application window has multiple persistent context marks")
	}
	if len(window.ContextMarks) == 1 {
		markedID := window.ContextMarks[0]
		for index := range registry.Contexts {
			context := registry.Contexts[index]
			if context.ID != markedID {
				continue
			}
			if context.App == nil || !applicationIdentitiesOverlap(context.App.Identity, window.Identity) {
				return ApplicationResolution{}, fmt.Errorf("application window is marked for context %q but its identity changed; use app rebind-focused", markedID)
			}
			copy := context
			result.Registered = &copy
			return result, nil
		}
		return ApplicationResolution{}, fmt.Errorf("application window has unknown persistent context mark %q", markedID)
	}
	for index := range registry.Contexts {
		context := registry.Contexts[index]
		if context.App == nil || !applicationIdentitiesOverlap(context.App.Identity, window.Identity) {
			continue
		}
		if result.Registered != nil {
			return ApplicationResolution{}, errors.New("application window overlaps multiple registered contexts")
		}
		copy := context
		result.Registered = &copy
	}
	return result, nil
}

// FocusedApplicationWindow returns the single focused application leaf.
func FocusedApplicationWindow(root *swayipc.TreeNode) (WindowApplication, error) {
	if root == nil {
		return WindowApplication{}, errors.New("sway tree is nil")
	}
	var matches []WindowApplication
	if err := walkApplicationWindows(root, "", func(window WindowApplication, focused bool) {
		if focused {
			matches = append(matches, window)
		}
	}); err != nil {
		return WindowApplication{}, err
	}
	switch len(matches) {
	case 0:
		return WindowApplication{}, errors.New("focused Sway container is not an eligible top-level application window")
	case 1:
		return matches[0], nil
	default:
		return WindowApplication{}, errors.New("sway tree contains multiple focused application windows")
	}
}

// FocusedWorkspaceApplications returns all eligible top-level windows on the
// currently focused normal workspace for previewed batch registration.
func FocusedWorkspaceApplications(root *swayipc.TreeNode) ([]WindowApplication, error) {
	focused, err := FocusedApplicationWindow(root)
	if err != nil {
		return nil, err
	}
	windows := make([]WindowApplication, 0)
	if err := walkApplicationWindows(root, "", func(window WindowApplication, _ bool) {
		if window.Workspace == focused.Workspace {
			windows = append(windows, window)
		}
	}); err != nil {
		return nil, err
	}
	sort.Slice(windows, func(left, right int) bool {
		if windows[left].ContainerID == focused.ContainerID {
			return true
		}
		if windows[right].ContainerID == focused.ContainerID {
			return false
		}
		return windows[left].ContainerID < windows[right].ContainerID
	})
	return windows, nil
}

// ApplicationWindows returns every eligible normal top-level on a regular
// workspace. Scratchpad windows remain outside the first desktop persistence
// release.
func ApplicationWindows(root *swayipc.TreeNode) ([]WindowApplication, error) {
	windows := make([]WindowApplication, 0)
	if err := walkApplicationWindows(root, "", func(window WindowApplication, _ bool) {
		windows = append(windows, window)
	}); err != nil {
		return nil, err
	}
	sort.Slice(windows, func(left, right int) bool {
		return windows[left].ContainerID < windows[right].ContainerID
	})
	return windows, nil
}

// ApplicationWindowByContainer re-observes one exact container before a
// short-lived approval is applied.
func ApplicationWindowByContainer(root *swayipc.TreeNode, containerID int64) (WindowApplication, error) {
	if root == nil || containerID <= 0 {
		return WindowApplication{}, errors.New("invalid Sway tree or container ID")
	}
	var matches []WindowApplication
	if err := walkApplicationWindows(root, "", func(window WindowApplication, _ bool) {
		if window.ContainerID == containerID {
			matches = append(matches, window)
		}
	}); err != nil {
		return WindowApplication{}, err
	}
	if len(matches) != 1 {
		return WindowApplication{}, fmt.Errorf("application container %d is no longer uniquely present", containerID)
	}
	return matches[0], nil
}

func walkApplicationWindows(node *swayipc.TreeNode, workspace string, visit func(WindowApplication, bool)) error {
	return walkApplicationWindowsWithOptions(node, workspace, applicationWindowWalkOptions{}, visit)
}

func walkApplicationWindowsWithScratchpad(node *swayipc.TreeNode, workspace string, includeScratchpad bool, visit func(WindowApplication, bool)) error {
	return walkApplicationWindowsWithOptions(node, workspace, applicationWindowWalkOptions{includeScratchpad: includeScratchpad}, visit)
}

func walkApplicationWindowsIncludingTransient(node *swayipc.TreeNode, workspace string, visit func(WindowApplication, bool)) error {
	return walkApplicationWindowsWithOptions(node, workspace, applicationWindowWalkOptions{includeScratchpad: true, includeRestoreStaging: true}, visit)
}

type applicationWindowWalkOptions struct {
	includeScratchpad     bool
	includeRestoreStaging bool
}

func walkApplicationWindowsWithOptions(node *swayipc.TreeNode, workspace string, options applicationWindowWalkOptions, visit func(WindowApplication, bool)) error {
	if node == nil {
		return errors.New("sway tree contains a nil node")
	}
	if node.Type == "workspace" {
		workspace = node.Name
	}
	if len(node.Nodes) == 0 && len(node.FloatingNodes) == 0 {
		identity, ok, err := compositorApplicationIdentity(node)
		if err != nil {
			return fmt.Errorf("container %d: %w", node.ID, err)
		}
		if ok && workspace != "" && (options.includeScratchpad || workspace != "__i3_scratch") && (options.includeRestoreStaging || workspace != RestoreStagingWorkspace) {
			marks, err := applicationContextMarks(node.Marks)
			if err != nil {
				return fmt.Errorf("container %d: %w", node.ID, err)
			}
			visit(WindowApplication{ContainerID: node.ID, Workspace: workspace, Identity: identity, ContextMarks: marks}, node.Focused)
		}
	}
	for _, child := range node.Nodes {
		if err := walkApplicationWindowsWithOptions(child, workspace, options, visit); err != nil {
			return err
		}
	}
	for _, child := range node.FloatingNodes {
		if err := walkApplicationWindowsWithOptions(child, workspace, options, visit); err != nil {
			return err
		}
	}
	return nil
}

func compositorApplicationIdentity(node *swayipc.TreeNode) (ApplicationIdentity, bool, error) {
	if node.ID <= 0 {
		return ApplicationIdentity{}, false, nil
	}
	if node.WindowProperties.TransientFor != nil || node.WindowProperties.WindowType != "" && node.WindowProperties.WindowType != "normal" {
		return ApplicationIdentity{}, false, nil
	}
	sandboxID := pointerString(node.SandboxAppID)
	if sandboxID != "" && !validFlatpakID(sandboxID) {
		return ApplicationIdentity{}, false, errors.New("sandbox application ID is not a valid Flatpak ID")
	}
	if appID := pointerString(node.AppID); appID != "" {
		identity := ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: appID, SandboxAppID: sandboxID}
		if err := identity.validate(); err != nil {
			return ApplicationIdentity{}, false, err
		}
		return identity, true, nil
	}
	class := node.WindowProperties.Class
	instance := node.WindowProperties.Instance
	if class == "" && instance == "" {
		return ApplicationIdentity{}, false, nil
	}
	identity := ApplicationIdentity{Protocol: WindowXWayland, X11Class: class, X11Instance: instance, SandboxAppID: sandboxID}
	if err := identity.validate(); err != nil {
		return ApplicationIdentity{}, false, err
	}
	return identity, true, nil
}

// DesktopCandidatesForWindow returns the exact launcher candidates without
// interpreting an existing context mark. Rebind uses it intentionally.
func DesktopCandidatesForWindow(window WindowApplication, catalog DesktopCatalog) []DesktopEntry {
	byID := make(map[string]DesktopEntry)
	for _, entry := range catalog.Entries() {
		if desktopEntryMatchesIdentity(entry, window.Identity) {
			byID[entry.ID] = entry
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]DesktopEntry, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	return result
}

// ApplicationIdentitiesOverlap reports whether two live identities represent
// the same application registration.
func ApplicationIdentitiesOverlap(left ApplicationIdentity, right ApplicationIdentity) bool {
	return applicationIdentitiesOverlap(left, right)
}

func applicationContextMarks(marks []string) ([]ContextID, error) {
	result := make([]ContextID, 0, 1)
	seen := make(map[ContextID]struct{})
	for _, mark := range marks {
		if !strings.HasPrefix(mark, MarkPrefix) {
			continue
		}
		id, err := ParseMark(mark)
		if err != nil {
			return nil, fmt.Errorf("invalid persistent context mark %q: %w", mark, err)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	if len(result) > 1 {
		return nil, errors.New("application window has multiple persistent context marks")
	}
	return result, nil
}

func desktopEntryMatchesIdentity(entry DesktopEntry, identity ApplicationIdentity) bool {
	if identity.SandboxAppID != "" {
		return entry.FlatpakID == identity.SandboxAppID && entry.FlatpakInstallation != ""
	}
	if entry.FlatpakID != "" {
		return false
	}
	stem := strings.TrimSuffix(entry.ID, ".desktop")
	switch identity.Protocol {
	case WindowWayland:
		return identity.WaylandAppID == stem || entry.StartupWMClass != "" && entry.StartupWMClass == identity.WaylandAppID
	case WindowXWayland:
		return stem == identity.X11Class || stem == identity.X11Instance ||
			entry.StartupWMClass != "" && (entry.StartupWMClass == identity.X11Class || entry.StartupWMClass == identity.X11Instance)
	default:
		return false
	}
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
