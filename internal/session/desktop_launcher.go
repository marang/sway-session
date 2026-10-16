package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

const (
	desktopApprovalDirectory = "desktop-approvals"
	systemGIOExecutable      = "/usr/bin/gio"
	systemFlatpakExecutable  = "/usr/bin/flatpak"
)

const maxApprovedExecutableSize = 1024 * 1024 * 1024

var indirectOrPrivilegedExecutables = map[string]struct{}{
	"bash": {}, "busybox": {}, "dash": {}, "doas": {}, "electron": {}, "env": {},
	"flatpak-spawn": {}, "java": {}, "node": {}, "nodejs": {}, "nohup": {}, "perl": {},
	"pkexec": {}, "ruby": {}, "run0": {}, "setsid": {}, "sh": {}, "sudo": {}, "su": {},
	"systemd-run": {}, "wine": {}, "wine64": {}, "zsh": {},
}

// ValidateDesktopEntryForApproval performs the launch-shape checks which can
// be shown during preview without trusting stale catalog filesystem metadata.
func ValidateDesktopEntryForApproval(entry DesktopEntry) error {
	return ValidateDesktopEntryForApprovalWithOptions(entry, DesktopApprovalOptions{})
}

func ValidateDesktopEntryForApprovalWithOptions(entry DesktopEntry, options DesktopApprovalOptions) error {
	if err := validateDesktopID(entry.ID); err != nil {
		return err
	}
	if entry.Name == "" || entry.Path == "" {
		return errors.New("desktop entry is incomplete")
	}
	if entry.FlatpakID != "" {
		launcher := Launcher{Kind: LauncherFlatpak, FlatpakID: entry.FlatpakID, FlatpakInstallation: entry.FlatpakInstallation}
		return launcher.validate()
	}
	if _, err := validateDesktopApprovalOptions(entry, options); err != nil {
		return err
	}
	if entry.Origin == DesktopEntryUser && entry.Exec == "" {
		return errors.New("user-local D-Bus-only desktop entries are unsupported")
	}
	return nil
}

// DesktopApprovalSummary is a bounded human-readable trust preview. It shows
// only launcher identity and executable origin, never arguments or URI fields.
func DesktopApprovalSummary(entry DesktopEntry) (string, error) {
	return DesktopApprovalSummaryWithOptions(entry, DesktopApprovalOptions{})
}

func DesktopApprovalSummaryWithOptions(entry DesktopEntry, options DesktopApprovalOptions) (string, error) {
	if err := ValidateDesktopEntryForApprovalWithOptions(entry, options); err != nil {
		return "", err
	}
	if entry.FlatpakID != "" {
		return fmt.Sprintf("%s (%s Flatpak %s)", entry.Name, entry.FlatpakInstallation, entry.FlatpakID), nil
	}
	executable := "D-Bus activation"
	if entry.Exec != "" {
		var err error
		executable, err = desktopExecExecutable(entry.Exec)
		if err != nil {
			return "", err
		}
	}
	summary := fmt.Sprintf("%s (%s desktop entry %s; executable %s)", entry.Name, entry.Origin, entry.Path, executable)
	if _, administrative, _ := administrativeDesktopEntry(entry); administrative {
		summary += "; administrative restore allowed: may request authentication again"
	}
	if len(summary) > 2048 {
		return "", errors.New("desktop approval summary exceeds 2048 bytes")
	}
	return summary, nil
}

// DesktopApproval is registration-time trust evidence plus the optional new
// immutable user-local desktop snapshot which must be removed if the registry
// transaction does not commit.
type DesktopApproval struct {
	Launcher        Launcher
	SnapshotPath    string
	SnapshotCreated bool
}

// PrepareDesktopApproval revalidates a catalog entry and derives one typed
// launcher. It never executes the desktop entry.
func PrepareDesktopApproval(stateRoot string, id ContextID, entry DesktopEntry) (DesktopApproval, error) {
	return PrepareDesktopApprovalContext(context.Background(), stateRoot, id, entry)
}

// PrepareDesktopApprovalContext is PrepareDesktopApproval with cancelable
// access to protected approval snapshots.
func PrepareDesktopApprovalContext(ctx context.Context, stateRoot string, id ContextID, entry DesktopEntry) (DesktopApproval, error) {
	return PrepareDesktopApprovalContextWithOptions(ctx, stateRoot, id, entry, DesktopApprovalOptions{})
}

func PrepareDesktopApprovalContextWithOptions(ctx context.Context, stateRoot string, id ContextID, entry DesktopEntry, options DesktopApprovalOptions) (DesktopApproval, error) {
	if ctx == nil {
		return DesktopApproval{}, errors.New("desktop approval context is nil")
	}
	if err := id.Validate(); err != nil {
		return DesktopApproval{}, err
	}
	if entry.FlatpakID != "" {
		launcher := Launcher{Kind: LauncherFlatpak, FlatpakID: entry.FlatpakID, FlatpakInstallation: entry.FlatpakInstallation}
		if err := launcher.validate(); err != nil {
			return DesktopApproval{}, err
		}
		return DesktopApproval{Launcher: launcher}, nil
	}
	data, resolved, parsed, err := revalidateCatalogDesktopEntry(entry)
	if err != nil {
		return DesktopApproval{}, err
	}
	administrative, err := validateDesktopApprovalOptions(parsed, options)
	if err != nil {
		return DesktopApproval{}, err
	}
	launcher := Launcher{
		Kind:          LauncherDesktop,
		DesktopID:     entry.ID,
		DesktopOrigin: entry.Origin,
		DesktopPath:   resolved,
	}
	if administrative {
		launcher.Administrative, err = prepareAdministrativeApproval(ctx, data, parsed)
		if err != nil {
			return DesktopApproval{}, err
		}
	}
	switch entry.Origin {
	case DesktopEntrySystem:
	case DesktopEntryUser:
		if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
			return DesktopApproval{}, errors.New("state root must be a clean absolute path")
		}
		digest := sha256Hex(data)
		launcher.DesktopEntrySHA256 = digest
		if !administrative {
			executable, owner, executableDigest, err := approvedDesktopExecutable(parsed.Exec)
			if err != nil {
				return DesktopApproval{}, err
			}
			if owner != 0 {
				launcher.ApprovedExecutablePath = executable
				launcher.ApprovedExecutableSHA256 = executableDigest
			}
		}
		directory := filepath.Join(stateRoot, desktopApprovalDirectory)
		name := string(id) + "-" + digest + ".desktop"
		created := false
		existing, readErr := statefile.ReadPrivateFileContext(ctx, directory, name)
		switch {
		case readErr == nil:
			if !reflect.DeepEqual(existing, data) {
				return DesktopApproval{}, errors.New("existing approved desktop snapshot does not match its content hash")
			}
		case errors.Is(readErr, os.ErrNotExist):
			if err := statefile.CreatePrivateFileContext(ctx, directory, name, data); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return DesktopApproval{}, fmt.Errorf("create approved desktop snapshot: %w", err)
				}
				existing, readErr = statefile.ReadPrivateFileContext(ctx, directory, name)
				if readErr != nil || !reflect.DeepEqual(existing, data) {
					return DesktopApproval{}, errors.New("concurrently created approved desktop snapshot does not match its content hash")
				}
			} else {
				created = true
			}
		default:
			return DesktopApproval{}, fmt.Errorf("inspect approved desktop snapshot: %w", readErr)
		}
		launcher.ApprovedDesktopPath = filepath.Join(directory, name)
		if err := launcher.validate(); err != nil {
			if created {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				_ = RemoveDesktopApprovalSnapshotContext(cleanupCtx, stateRoot, launcher)
				cancel()
			}
			return DesktopApproval{}, err
		}
		return DesktopApproval{Launcher: launcher, SnapshotPath: launcher.ApprovedDesktopPath, SnapshotCreated: created}, nil
	default:
		return DesktopApproval{}, fmt.Errorf("unsupported desktop entry origin %q", entry.Origin)
	}
	if err := launcher.validate(); err != nil {
		return DesktopApproval{}, err
	}
	return DesktopApproval{Launcher: launcher, SnapshotPath: launcher.ApprovedDesktopPath}, nil
}

// DiscardDesktopApproval removes only a snapshot created by this uncommitted
// approval and only while it remains unreferenced by the registry.
func DiscardDesktopApproval(stateRoot string, approval DesktopApproval) error {
	return DiscardDesktopApprovalContext(context.Background(), stateRoot, approval)
}

// DiscardDesktopApprovalContext is DiscardDesktopApproval with cancelable
// protected snapshot access.
func DiscardDesktopApprovalContext(ctx context.Context, stateRoot string, approval DesktopApproval) error {
	if !approval.SnapshotCreated {
		return nil
	}
	return RemoveDesktopApprovalSnapshotContext(ctx, stateRoot, approval.Launcher)
}

// RemoveDesktopApprovalSnapshot removes only an unreferenced snapshot proven
// to be inside this application's private approval directory.
func RemoveDesktopApprovalSnapshot(stateRoot string, launcher Launcher) error {
	return RemoveDesktopApprovalSnapshotContext(context.Background(), stateRoot, launcher)
}

// RemoveDesktopApprovalSnapshotContext is RemoveDesktopApprovalSnapshot with
// cancelable protected snapshot and registry access. The registry lock keeps a
// concurrent commit from beginning after the reference check but before the
// snapshot removal.
func RemoveDesktopApprovalSnapshotContext(ctx context.Context, stateRoot string, launcher Launcher) error {
	if ctx == nil {
		return errors.New("desktop approval removal context is nil")
	}
	if launcher.ApprovedDesktopPath == "" {
		return nil
	}
	directory := filepath.Join(stateRoot, desktopApprovalDirectory)
	if filepath.Dir(launcher.ApprovedDesktopPath) != directory {
		return errors.New("approved desktop snapshot is outside the private approval directory")
	}
	return InspectRegistryLockedContext(ctx, stateRoot, func(registry Registry) error {
		if registryReferencesDesktopApproval(registry, launcher.ApprovedDesktopPath) {
			return nil
		}
		referenced, err := LifecycleReferencesDesktopApprovalContext(ctx, stateRoot, launcher.ApprovedDesktopPath)
		if err != nil {
			return err
		}
		if referenced {
			return nil
		}
		return statefile.RemovePrivateFileContext(ctx, directory, filepath.Base(launcher.ApprovedDesktopPath))
	})
}

func validateUnreferencedDesktopApproval(ctx context.Context, stateRoot string, registry Registry, launcher Launcher) error {
	if launcher.Kind != LauncherDesktop || launcher.DesktopOrigin != DesktopEntryUser ||
		registryReferencesDesktopApproval(registry, launcher.ApprovedDesktopPath) {
		return nil
	}
	directory := filepath.Join(stateRoot, desktopApprovalDirectory)
	if filepath.Dir(launcher.ApprovedDesktopPath) != directory {
		return errors.New("approved desktop snapshot is outside the private approval directory")
	}
	snapshot, err := statefile.ReadPrivateFileContext(ctx, directory, filepath.Base(launcher.ApprovedDesktopPath))
	if err != nil {
		return fmt.Errorf("read approved desktop snapshot before registry commit: %w", err)
	}
	if sha256Hex(snapshot) != launcher.DesktopEntrySHA256 {
		return errors.New("approved desktop snapshot changed before registry commit")
	}
	return nil
}

func registryReferencesDesktopApproval(registry Registry, path string) bool {
	for _, context := range registry.Contexts {
		if context.Launcher.ApprovedDesktopPath == path {
			return true
		}
	}
	return false
}

// DesktopApplicationLauncher starts only the two typed desktop launcher forms.
// The executable paths are supplied by the root-owned system resolver.
type DesktopApplicationLauncher struct {
	GIO           string
	Flatpak       string
	StateRoot     string
	Starter       ProcessStarter
	VerifyFlatpak func(Launcher) error
	// ObservationRunner and ObservationResolveSystem are read-only,
	// cancellation-aware inventory boundaries, independent of Launch.
	ObservationRunner        CommandOutputRunner
	ObservationResolveSystem func(string) (string, error)
}

func (launcher DesktopApplicationLauncher) Launch(context Context) error {
	if launcher.Starter == nil {
		return errors.New("process starter is nil")
	}
	if err := context.Validate(); err != nil {
		return fmt.Errorf("validate context: %w", err)
	}
	if context.Launcher.Kind == LauncherFlatpak {
		if launcher.VerifyFlatpak == nil {
			return errors.New("flatpak installation verifier is nil")
		}
		if err := launcher.VerifyFlatpak(context.Launcher); err != nil {
			return err
		}
	}
	spec, err := launcher.Spec(context)
	if err != nil {
		return err
	}
	return launcher.Starter.Start(spec)
}

// Spec revalidates all mutable trust evidence immediately before returning a
// no-shell process specification.
func (launcher DesktopApplicationLauncher) Spec(sessionContext Context) (ProcessSpec, error) {
	return launcher.SpecContext(context.Background(), sessionContext)
}

// SpecContext is Spec with cancellation for mutable approval and executable
// revalidation performed immediately before launch.
func (launcher DesktopApplicationLauncher) SpecContext(ctx context.Context, sessionContext Context) (ProcessSpec, error) {
	if ctx == nil {
		return ProcessSpec{}, errors.New("desktop launch context is nil")
	}
	if err := ctx.Err(); err != nil {
		return ProcessSpec{}, err
	}
	if err := sessionContext.Validate(); err != nil {
		return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "launcher_context_invalid", fmt.Errorf("validate context: %w", err))
	}
	switch sessionContext.Launcher.Kind {
	case LauncherDesktop:
		if sessionContext.Launcher.Administrative == nil && launcher.GIO != systemGIOExecutable {
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "launcher_command_invalid", fmt.Errorf("gio launcher must be %s", systemGIOExecutable))
		}
		return revalidateDesktopLaunchContext(ctx, sessionContext.Launcher, launcher.StateRoot)
	case LauncherFlatpak:
		if launcher.Flatpak != systemFlatpakExecutable {
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "launcher_command_invalid", fmt.Errorf("flatpak launcher must be %s", systemFlatpakExecutable))
		}
		installation := "--system"
		if sessionContext.Launcher.FlatpakInstallation == FlatpakUser {
			installation = "--user"
		}
		return ProcessSpec{Name: launcher.Flatpak, Arguments: []string{"run", installation, sessionContext.Launcher.FlatpakID}, Environment: []string{"PATH=/usr/local/bin:/usr/bin"}}, nil
	default:
		return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "launcher_kind_invalid", fmt.Errorf("desktop launcher does not support context launcher kind %q", sessionContext.Launcher.Kind))
	}
}

func revalidateDesktopLaunchContext(ctx context.Context, launcher Launcher, stateRoot string) (ProcessSpec, error) {
	if ctx == nil {
		return ProcessSpec{}, errors.New("desktop launch context is nil")
	}
	if err := ctx.Err(); err != nil {
		return ProcessSpec{}, err
	}
	switch launcher.DesktopOrigin {
	case DesktopEntrySystem:
		data, err := readTrustedRegularFile(launcher.DesktopPath, 0, true, MaxDesktopEntrySize)
		if err != nil {
			return ProcessSpec{}, launcherFileFailure(fmt.Errorf("system desktop entry changed trust: %w", err), ApplicationLauncherInvalid, "desktop_entry_untrusted", "desktop_entry_missing")
		}
		parsed, hidden, err := parseDesktopEntry(data)
		if err != nil || hidden {
			if err == nil {
				err = errors.New("desktop entry is hidden")
			}
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "desktop_entry_invalid", err)
		}
		if launcher.Administrative != nil {
			if err := revalidateAdministrativeApproval(ctx, launcher, data, parsed); err != nil {
				return ProcessSpec{}, err
			}
		} else if err := rejectAdministrativeDesktopEntry(parsed); err != nil {
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "desktop_entry_invalid", err)
		}
		return desktopProcessSpec(launcher, parsed, launcher.DesktopPath)
	case DesktopEntryUser:
		directory := filepath.Join(stateRoot, desktopApprovalDirectory)
		if filepath.Dir(launcher.ApprovedDesktopPath) != directory {
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "approval_invalid", errors.New("approved desktop snapshot is outside the private approval directory"))
		}
		source, err := readTrustedRegularFile(launcher.DesktopPath, uint32(os.Geteuid()), false, MaxDesktopEntrySize)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return ProcessSpec{}, contextErr
			}
			classified := launcherFileFailure(err, ApplicationLauncherReapprovalRequired, "desktop_entry_changed", "desktop_entry_missing")
			return ProcessSpec{}, &launcherValidationError{classified.observation, errors.New("user desktop entry changed and requires explicit reapproval")}
		}
		if sha256Hex(source) != launcher.DesktopEntrySHA256 {
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherReapprovalRequired, "desktop_entry_changed", errors.New("user desktop entry changed and requires explicit reapproval"))
		}
		snapshot, err := statefile.ReadPrivateFileContext(ctx, directory, filepath.Base(launcher.ApprovedDesktopPath))
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return ProcessSpec{}, contextErr
			}
			if errors.Is(err, os.ErrPermission) {
				return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherUnknown, "permission_denied", errors.New("approved desktop snapshot is missing or changed"))
			}
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherReapprovalRequired, "approval_missing_or_changed", errors.New("approved desktop snapshot is missing or changed"))
		}
		if sha256Hex(snapshot) != launcher.DesktopEntrySHA256 {
			return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherReapprovalRequired, "approval_missing_or_changed", errors.New("approved desktop snapshot is missing or changed"))
		}
		var parsed DesktopEntry
		if launcher.Administrative != nil {
			var hidden bool
			parsed, hidden, err = parseDesktopEntry(snapshot)
			if err != nil || hidden {
				return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherReapprovalRequired, "approval_missing_or_changed", errors.New("administrative desktop snapshot is invalid"))
			}
			if err := revalidateAdministrativeApproval(ctx, launcher, snapshot, parsed); err != nil {
				return ProcessSpec{}, err
			}
		}
		if launcher.ApprovedExecutablePath != "" {
			digest, err := hashTrustedExecutableContext(ctx, launcher.ApprovedExecutablePath, uint32(os.Geteuid()), false)
			if err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return ProcessSpec{}, contextErr
				}
				classified := launcherFileFailure(err, ApplicationLauncherReapprovalRequired, "approved_executable_changed", "approved_executable_missing")
				return ProcessSpec{}, &launcherValidationError{classified.observation, errors.New("approved user executable changed and requires explicit reapproval")}
			}
			if digest != launcher.ApprovedExecutableSHA256 {
				return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherReapprovalRequired, "approved_executable_changed", errors.New("approved user executable changed and requires explicit reapproval"))
			}
		}
		return desktopProcessSpec(launcher, parsed, launcher.ApprovedDesktopPath)
	default:
		return ProcessSpec{}, launcherValidationFailure(ApplicationLauncherInvalid, "desktop_origin_invalid", fmt.Errorf("unsupported desktop entry origin %q", launcher.DesktopOrigin))
	}
}

func revalidateCatalogDesktopEntry(entry DesktopEntry) ([]byte, string, DesktopEntry, error) {
	if err := validateDesktopID(entry.ID); err != nil {
		return nil, "", DesktopEntry{}, err
	}
	resolved, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, "", DesktopEntry{}, err
	}
	resolved = filepath.Clean(resolved)
	owner := uint32(0)
	requireRootAncestors := true
	if entry.Origin == DesktopEntryUser {
		owner = uint32(os.Geteuid())
		requireRootAncestors = false
	} else if entry.Origin != DesktopEntrySystem {
		return nil, "", DesktopEntry{}, fmt.Errorf("unsupported desktop entry origin %q", entry.Origin)
	}
	data, err := readTrustedRegularFile(resolved, owner, requireRootAncestors, MaxDesktopEntrySize)
	if err != nil {
		return nil, "", DesktopEntry{}, fmt.Errorf("desktop entry is unsafe: %w", err)
	}
	parsed, hidden, err := parseDesktopEntry(data)
	if err != nil {
		return nil, "", DesktopEntry{}, err
	}
	if hidden {
		return nil, "", DesktopEntry{}, errors.New("desktop entry became hidden")
	}
	parsed.ID, parsed.Path, parsed.Origin = entry.ID, entry.Path, entry.Origin
	parsed.FlatpakInstallation = entry.FlatpakInstallation
	if !sameDesktopLaunchMaterial(parsed, entry) {
		return nil, "", DesktopEntry{}, errors.New("desktop entry changed while registration approval was pending")
	}
	return data, filepath.Clean(resolved), parsed, nil
}

func sameDesktopLaunchMaterial(left DesktopEntry, right DesktopEntry) bool {
	left.Path, right.Path = "", ""
	return reflect.DeepEqual(left, right)
}

func rejectAdministrativeDesktopEntry(entry DesktopEntry) error {
	if entry.Exec == "" {
		if entry.DBusActivatable {
			return nil
		}
		return errors.New("desktop entry has no executable launch material")
	}
	executable, err := desktopExecExecutable(entry.Exec)
	if err != nil {
		return err
	}
	if isIndirectOrPrivilegedExecutable(executable) {
		return fmt.Errorf("desktop entry uses unsupported administrative or indirect executable %q", filepath.Base(executable))
	}
	return nil
}

func approvedDesktopExecutable(execValue string) (string, uint32, string, error) {
	if execValue == "" {
		return "", 0, "", errors.New("user-local D-Bus-only desktop entries are unsupported")
	}
	token, err := desktopExecExecutable(execValue)
	if err != nil {
		return "", 0, "", err
	}
	if isIndirectOrPrivilegedExecutable(token) {
		return "", 0, "", fmt.Errorf("user desktop entry uses unsupported indirect executable %q", filepath.Base(token))
	}
	var candidate string
	if filepath.IsAbs(token) {
		candidate = token
	} else if filepath.Base(token) == token {
		for _, directory := range []string{"/usr/local/bin", "/usr/bin"} {
			path := filepath.Join(directory, token)
			if _, err := os.Stat(path); err == nil {
				candidate = path
				break
			}
		}
		if candidate == "" {
			return "", 0, "", errors.New("user desktop entry executable must be absolute unless it resolves to a system binary")
		}
	} else {
		return "", 0, "", errors.New("desktop entry executable must be an absolute path or base name")
	}
	resolved := filepath.Clean(candidate)
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", 0, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", 0, "", errors.New("desktop executable path must not be a symlink")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return "", 0, "", errors.New("desktop executable must be a non-writable executable regular file")
	}
	if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return "", 0, "", fmt.Errorf("desktop executable is owned by untrusted uid %d", stat.Uid)
	}
	digest, err := hashTrustedExecutable(resolved, stat.Uid, stat.Uid == 0)
	if err != nil {
		return "", 0, "", err
	}
	return resolved, stat.Uid, digest, nil
}

func isIndirectOrPrivilegedExecutable(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if _, denied := indirectOrPrivilegedExecutables[name]; denied {
		return true
	}
	return strings.HasPrefix(name, "python") || strings.HasPrefix(name, "ruby") || strings.HasPrefix(name, "perl")
}

func desktopExecExecutable(value string) (string, error) {
	words, err := desktopExecWords(value)
	if err != nil {
		return "", err
	}
	if len(words) == 0 || words[0] == "" {
		return "", errors.New("desktop entry Exec has no executable token")
	}
	executable := words[0]
	if strings.Contains(executable, "%") || containsControl(executable) {
		return "", errors.New("desktop entry executable token contains a field code or control character")
	}
	return executable, nil
}

// Parse the desktop-entry key-file/quote/escape syntax only to identify the
// helper and payload and to build the bounded administrative argv. It never
// evaluates shell syntax; ordinary desktop execution remains delegated to GIO.
func desktopExecWords(value string) ([]string, error) {
	return parseDesktopExecWords(value, false)
}

func parseDesktopExecWords(value string, rejectUnquotedHash bool) ([]string, error) {
	value, err := unescapeDesktopString(value)
	if err != nil {
		return nil, fmt.Errorf("decode desktop Exec: %w", err)
	}
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, ch := range value {
		if ch < 0x20 && ch != '\t' || ch == 0x7f {
			return nil, errors.New("desktop Exec contains a control character")
		}
		if escaped {
			if quote == '"' && !strings.ContainsRune("\\\"$`", ch) {
				word.WriteRune('\\')
			}
			word.WriteRune(ch)
			escaped, started = false, true
			continue
		}
		if rejectUnquotedHash && quote == 0 && ch == '#' {
			return nil, errors.New("administrative desktop Exec must quote or escape literal # characters; comments are unsupported")
		}
		switch {
		case ch == '\\' && quote != '\'':
			escaped, started = true, true
		case ch == '\'' || ch == '"':
			if quote == 0 {
				quote = ch
			} else if quote == ch {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			started = true
		case quote == 0 && (ch == ' ' || ch == '\t'):
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("desktop Exec has incomplete quoting")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

func openTrustedRegular(path string, owner uint32, requireRootAncestors bool) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("path must be a clean absolute path")
	}
	rootFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	currentFile := os.NewFile(uintptr(rootFD), string(filepath.Separator))
	if currentFile == nil {
		_ = unix.Close(rootFD)
		return nil, errors.New("open trusted path root: invalid file descriptor")
	}
	currentPath := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		last := index == len(components)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !last {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(int(currentFile.Fd()), component, flags, 0)
		if err != nil {
			_ = currentFile.Close()
			return nil, err
		}
		nextPath := filepath.Join(currentPath, component)
		nextFile := os.NewFile(uintptr(fd), nextPath)
		if nextFile == nil {
			_ = unix.Close(fd)
			_ = currentFile.Close()
			return nil, errors.New("open trusted path: invalid file descriptor")
		}
		info, err := nextFile.Stat()
		if err != nil {
			_ = nextFile.Close()
			_ = currentFile.Close()
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			_ = nextFile.Close()
			_ = currentFile.Close()
			return nil, errors.New("path ownership metadata is unavailable")
		}
		stickyRootDirectory := info.IsDir() && info.Mode()&os.ModeSticky != 0 && stat.Uid == 0
		if info.Mode().Perm()&0o022 != 0 && !stickyRootDirectory {
			_ = nextFile.Close()
			_ = currentFile.Close()
			return nil, fmt.Errorf("path component %s is group- or world-writable", nextPath)
		}
		if requireRootAncestors {
			if stat.Uid != 0 {
				_ = nextFile.Close()
				_ = currentFile.Close()
				return nil, fmt.Errorf("path component %s is not root-owned", nextPath)
			}
		} else if last && stat.Uid != owner {
			_ = nextFile.Close()
			_ = currentFile.Close()
			return nil, fmt.Errorf("file is not owned by uid %d", owner)
		}
		if last {
			_ = currentFile.Close()
			if !info.Mode().IsRegular() {
				_ = nextFile.Close()
				return nil, errors.New("trusted path target must be a regular file")
			}
			return nextFile, nil
		}
		_ = currentFile.Close()
		currentFile = nextFile
		currentPath = nextPath
	}
	_ = currentFile.Close()
	return nil, errors.New("trusted path target is missing")
}

func readTrustedRegularFile(path string, owner uint32, requireRootAncestors bool, maximum int64) ([]byte, error) {
	file, err := openTrustedRegular(path, owner, requireRootAncestors)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("trusted file exceeds %d bytes", maximum)
	}
	return data, nil
}

func hashTrustedExecutable(path string, owner uint32, requireRootAncestors bool) (string, error) {
	return hashTrustedExecutableContext(context.Background(), path, owner, requireRootAncestors)
}

func hashTrustedExecutableContext(ctx context.Context, path string, owner uint32, requireRootAncestors bool) (string, error) {
	if ctx == nil {
		return "", errors.New("approved executable context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := openTrustedRegular(path, owner, requireRootAncestors)
	if err != nil {
		return "", fmt.Errorf("open approved executable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("approved executable is no longer executable")
	}
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	limited := io.LimitReader(file, maxApprovedExecutableSize+1)
	written := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read, readErr := limited.Read(buffer)
		if read > 0 {
			written += int64(read)
			if _, err := hash.Write(buffer[:read]); err != nil {
				return "", fmt.Errorf("hash approved executable: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("hash approved executable: %w", readErr)
		}
	}
	if written > maxApprovedExecutableSize {
		return "", fmt.Errorf("approved executable exceeds %d bytes", maxApprovedExecutableSize)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type CommandOutputRunner interface {
	CombinedOutput(context.Context, string, ...string) ([]byte, error)
}

// VerifyFlatpakInstallation checks the exact typed installation immediately
// before registration or launch.
func VerifyFlatpakInstallation(executable string, launcher Launcher, runner CommandOutputRunner) error {
	return VerifyFlatpakInstallationContext(context.Background(), executable, launcher, runner)
}

// VerifyFlatpakInstallationContext bounds the Flatpak probe by both the
// caller's lifecycle and the operation-specific timeout.
func VerifyFlatpakInstallationContext(parent context.Context, executable string, launcher Launcher, runner CommandOutputRunner) error {
	if parent == nil {
		return errors.New("flatpak verification context is nil")
	}
	if executable != systemFlatpakExecutable || runner == nil {
		return fmt.Errorf("flatpak verifier requires %s and a runner", systemFlatpakExecutable)
	}
	if launcher.Kind != LauncherFlatpak {
		return errors.New("flatpak verifier requires a Flatpak launcher")
	}
	if err := launcher.validate(); err != nil {
		return err
	}
	installation := "--system"
	if launcher.FlatpakInstallation == FlatpakUser {
		installation = "--user"
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if _, err := runner.CombinedOutput(ctx, executable, "info", installation, launcher.FlatpakID); err != nil {
		return fmt.Errorf("verify installed Flatpak %s (%s): %w", launcher.FlatpakID, launcher.FlatpakInstallation, err)
	}
	return nil
}
