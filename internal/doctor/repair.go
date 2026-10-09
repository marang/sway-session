package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	doctorSnippetName = "50-sway-session-doctor.conf"
	doctorHeader      = "# Managed by sway-session doctor. Manual edits disable automatic repair.\n" +
		"# sway-session-doctor-format: 1\n"
	temporaryAttempts = 32
)

type fileEdit struct {
	path       string
	oldContent []byte
	newContent []byte
	oldState   safeFileState
	existed    bool
	directory  safeDirectoryState
	mode       os.FileMode
	preview    string
	observed   []configFingerprint
}

type safeDirectoryState struct {
	device uint64
	inode  uint64
	owner  uint32
	mode   os.FileMode
}

// A receipt identifies the actual inode installed by this attempt. Equal bytes
// alone never prove ownership of a file created by a different writer.
type appliedFileEdit struct {
	edit      fileEdit
	installed safeFileState
}

// A failed compensation left a displaced file or an unverified destination.
// Keep that uncertainty visible even when earlier writes roll back cleanly.
type unresolvedRepairError struct{ error }

func (err *unresolvedRepairError) Unwrap() error { return err.error }

func (service *Service) Plan(ctx context.Context, fixID string, request RepairOptions) (Plan, error) {
	if fixID != swayIntegrationFixID {
		return Plan{}, fmt.Errorf("unknown repair %q", fixID)
	}
	switch request.Shortcuts {
	case ShortcutsUnspecified, ShortcutsNone, ShortcutsDefault:
	default:
		return Plan{}, fmt.Errorf("unknown shortcut selection %q", request.Shortcuts)
	}
	analysis, err := analyzeSwayConfig(ctx, service.options)
	if err != nil {
		return Plan{}, err
	}
	if analysis.snippetErr != nil {
		return Plan{}, fmt.Errorf("refuse to update doctor-managed snippet; move or remove it manually first: %w", analysis.snippetErr)
	}
	if analysis.snippet.legacy {
		return Plan{}, errors.New("legacy partial profile requires manual migration; move or remove it before adopting the standard integration")
	}
	if (!analysis.snippetExists || analysis.includeLine == 0) && !request.AdoptStandard {
		return Plan{}, errors.New("explicit adoption of the standard integration is required to create or recover its file or append a direct include; review previous integration and load order manually")
	}
	directory, err := repairDirectory(analysis)
	if err != nil {
		return Plan{}, fmt.Errorf("validate Sway repair ownership and directory: %w", err)
	}
	executable := analysis.snippet.executable
	if !analysis.snippetExists {
		executable, err = repairExecutable(service.options.Executable)
		if err != nil {
			return Plan{}, err
		}
	}
	shortcuts := request.Shortcuts
	if shortcuts == ShortcutsUnspecified {
		shortcuts = ShortcutsNone
		if analysis.snippetExists {
			shortcuts = analysis.snippet.shortcuts
		}
	}
	kinds := []integrationKind{integrationDaemon, integrationRestore}
	if shortcuts == ShortcutsDefault {
		kinds = integrationOrder
	}
	newSnippet := renderManagedSnippet(executable, kinds)
	common := fileEdit{directory: directory, observed: slices.Clone(analysis.observed)}
	plan := Plan{ID: swayIntegrationFixID,
		Summary: "Configure the owned standard Sway integration; review previous integration and effective load order manually.",
		request: request, fixID: fixID, trusted: true}
	if !analysis.snippetExists || !bytes.Equal(analysis.snippetContent, newSnippet) {
		edit := common
		edit.path = analysis.snippetPath
		edit.oldContent = slices.Clone(analysis.snippetContent)
		edit.newContent = newSnippet
		edit.oldState = analysis.snippetState
		edit.existed = analysis.snippetExists
		edit.mode = 0o600
		if edit.existed {
			edit.mode = analysis.snippetState.mode
		}
		edit.preview = string(newSnippet)
		plan.edits = append(plan.edits, edit)
		plan.Changes = append(plan.Changes, FileChange{Path: edit.path, Preview: edit.preview})
	}
	if analysis.includeLine == 0 {
		includeLine, err := renderIncludeLine(analysis.snippetPath)
		if err != nil {
			return Plan{}, err
		}
		edit := common
		edit.path = analysis.root
		edit.oldContent = slices.Clone(analysis.rootContent)
		edit.newContent = appendConfigLine(analysis.rootContent, includeLine)
		if len(edit.newContent) > maxSwayConfigBytes {
			return Plan{}, errors.New("appending the direct include would exceed the supported configuration size")
		}
		edit.oldState = analysis.rootState
		edit.existed = true
		edit.mode = analysis.rootState.mode
		edit.preview = "+ " + includeLine + "\n"
		plan.edits = append(plan.edits, edit)
		plan.Changes = append(plan.Changes, FileChange{Path: edit.path, Preview: edit.preview})
	}
	if len(plan.edits) == 0 {
		return Plan{}, errors.New("standard Sway integration already has the requested profile; repair is not needed")
	}
	return plan, nil
}

func (service *Service) Apply(ctx context.Context, plan Plan) (FixResult, error) {
	if !plan.trusted || plan.fixID != swayIntegrationFixID || len(plan.edits) == 0 {
		return FixResult{}, errors.New("repair plan is missing trusted private edit data")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return FixResult{}, err
	}
	fresh, err := service.Plan(ctx, plan.fixID, plan.request)
	if err != nil {
		return FixResult{}, fmt.Errorf("repair plan is stale: %w", err)
	}
	if !sameRepairPlan(plan, fresh) {
		return FixResult{}, errors.New("repair plan is stale; preview the repair again")
	}
	for _, edit := range plan.edits {
		if err := revalidateEdit(edit); err != nil {
			return FixResult{}, fmt.Errorf("repair plan is stale for %s: %w", edit.path, err)
		}
	}

	backups := make([]string, len(plan.edits))
	for index, edit := range plan.edits {
		if !edit.existed {
			continue
		}
		backup, err := createExclusiveBackup(edit)
		if err != nil {
			return FixResult{}, fmt.Errorf("create 0600 backup for %s: %w", edit.path, err)
		}
		backups[index] = backup
	}

	var applied []appliedFileEdit
	for _, edit := range plan.edits {
		if err := ctx.Err(); err != nil {
			return FixResult{}, service.rollbackFailure(applied, backups, err)
		}
		// Advance only fingerprints for our own proven earlier writes. Every
		// other observed file must still match the preview during this write.
		edit.observed = slices.Clone(edit.observed)
		for index, observed := range edit.observed {
			for _, receipt := range applied {
				if observed.path == receipt.edit.path {
					edit.observed[index].state = receipt.installed
					edit.observed[index].digest = sha256.Sum256(receipt.edit.newContent)
				}
			}
		}
		receipt, err := atomicWriteEdit(edit)
		if receipt.installed.inode != 0 {
			applied = append(applied, receipt)
		}
		if err != nil {
			return FixResult{}, service.rollbackFailure(applied, backups,
				fmt.Errorf("apply %s: %w", edit.path, err))
		}
	}

	resultBackups := make([]string, 0, len(backups))
	for _, backup := range backups {
		if backup != "" {
			resultBackups = append(resultBackups, backup)
		}
	}
	message := "Applied the owned standard Sway integration files. Reload Sway manually to load binding changes; removing owned bindings does not establish which prior bindings will become active. Startup commands run in the next Sway session; reload does not run them. Review previous integration and effective load order manually."
	return FixResult{
		ID:      swayIntegrationFixID,
		Message: message,
		Backups: resultBackups,
	}, nil
}

func (service *Service) rollbackFailure(applied []appliedFileEdit, backups []string, cause error) error {
	var unresolved *unresolvedRepairError
	uncertain := errors.As(cause, &unresolved)
	var rollbackErrors []error
	for index := len(applied) - 1; index >= 0; index-- {
		receipt := applied[index]
		edit := receipt.edit
		current, state, exists, err := readOptionalRepairFile(edit.path)
		if err != nil {
			uncertain = true
			continue
		}
		if !edit.existed && !exists {
			continue
		}
		if !exists || state != receipt.installed || !bytes.Equal(current, edit.newContent) {
			uncertain = true
			continue
		}
		if edit.existed {
			restore := edit
			restore.observed = nil
			restore.oldContent = edit.newContent
			restore.oldState = receipt.installed
			restore.newContent = edit.oldContent
			restore.mode = edit.oldState.mode
			if _, err := atomicWriteEdit(restore); err != nil {
				uncertain = true
				rollbackErrors = append(rollbackErrors, err)
			}
			continue
		}
		if err := unlinkProvenFile(receipt); err != nil {
			uncertain = true
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	hints := nonemptyStrings(backups)
	cause = errors.Join(append([]error{cause}, rollbackErrors...)...)
	if uncertain {
		if len(hints) != 0 {
			return fmt.Errorf("%w; rollback could not be proven complete; preserved backups: %s", cause, strings.Join(hints, ", "))
		}
		return fmt.Errorf("%w; rollback could not be proven complete; inspect the managed files before retrying", cause)
	}
	if len(hints) != 0 {
		return fmt.Errorf("%w; applied files were rolled back; backups remain at %s", cause, strings.Join(hints, ", "))
	}
	return fmt.Errorf("%w; applied files were rolled back", cause)
}

func repairExecutable(value string) (string, error) {
	if value == "" {
		return "/usr/bin/sway-session", nil
	}
	if _, err := cleanAbsolutePath(value); err != nil {
		return "", fmt.Errorf("repair executable: %w", err)
	}
	if filepath.Base(value) != "sway-session" {
		return "", errors.New("repair executable must name the sway-session program")
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("/_+.-", character) {
			continue
		}
		return "", fmt.Errorf("repair executable %q contains characters unsafe for a Sway command", value)
	}
	return value, nil
}

func renderManagedSnippet(executable string, kinds []integrationKind) []byte {
	var result strings.Builder
	result.WriteString(doctorHeader)
	for _, kind := range integrationOrder {
		if !slices.Contains(kinds, kind) {
			continue
		}
		result.WriteString(integrationDirective(executable, kind))
		result.WriteByte('\n')
	}
	return []byte(result.String())
}

func integrationDirective(executable string, kind integrationKind) string {
	switch kind {
	case integrationDaemon:
		return "exec --no-startup-id " + executable + " daemon"
	case integrationRestore:
		return "exec --no-startup-id " + executable + " restore"
	case integrationPersistent:
		return "bindsym $mod+Return exec --no-startup-id " + executable + " terminal --new"
	case integrationEphemeral:
		return "bindsym $mod+Shift+Return exec --no-startup-id " + executable + " terminal --ephemeral"
	default:
		panic("unknown integration kind")
	}
}

func parseManagedSnippet(content []byte) (managedSnippet, error) {
	if !bytes.HasPrefix(content, []byte(doctorHeader)) {
		return managedSnippet{}, errors.New("file does not have the exact doctor ownership header and format version")
	}
	if len(content) > maxSwayConfigBytes {
		return managedSnippet{}, errors.New("file exceeds the supported size")
	}
	remainder := string(content[len(doctorHeader):])
	kinds := []integrationKind(nil)
	executable := ""
	if remainder != "" {
		if !strings.HasSuffix(remainder, "\n") {
			return managedSnippet{}, errors.New("file contains unrecognized manual edits")
		}
		for _, line := range strings.Split(strings.TrimSuffix(remainder, "\n"), "\n") {
			kind, lineExecutable, ok := parseExactManagedDirective(strings.Fields(line))
			if !ok || line != integrationDirective(lineExecutable, kind) {
				return managedSnippet{}, errors.New("file contains unrecognized manual edits")
			}
			if executable != "" && executable != lineExecutable {
				return managedSnippet{}, errors.New("file contains inconsistent managed executable paths")
			}
			executable = lineExecutable
			if slices.Contains(kinds, kind) {
				return managedSnippet{}, errors.New("file contains duplicate managed directives")
			}
			kinds = append(kinds, kind)
		}
	}
	if !bytes.Equal(content, renderManagedSnippet(executable, kinds)) {
		return managedSnippet{}, errors.New("file contains unrecognized manual edits or reordered directives")
	}
	result := managedSnippet{executable: executable, legacy: true}
	if slices.Equal(kinds, []integrationKind{integrationDaemon, integrationRestore}) {
		result.shortcuts, result.legacy = ShortcutsNone, false
	} else if slices.Equal(kinds, integrationOrder) {
		result.shortcuts, result.legacy = ShortcutsDefault, false
	}
	return result, nil
}

func parseExactManagedDirective(tokens []string) (integrationKind, string, bool) {
	if len(tokens) == 4 && tokens[0] == "exec" && tokens[1] == "--no-startup-id" {
		if executable, err := repairExecutable(tokens[2]); err == nil {
			switch tokens[3] {
			case "daemon":
				return integrationDaemon, executable, true
			case "restore":
				return integrationRestore, executable, true
			}
		}
	}
	if len(tokens) == 7 && tokens[0] == "bindsym" && tokens[2] == "exec" && tokens[3] == "--no-startup-id" && tokens[5] == "terminal" {
		executable, err := repairExecutable(tokens[4])
		if err != nil {
			return 0, "", false
		}
		switch {
		case tokens[1] == "$mod+Return" && tokens[6] == "--new":
			return integrationPersistent, executable, true
		case tokens[1] == "$mod+Shift+Return" && tokens[6] == "--ephemeral":
			return integrationEphemeral, executable, true
		}
	}
	return 0, "", false
}

func renderIncludeLine(path string) (string, error) {
	if _, err := cleanAbsolutePath(path); err != nil {
		return "", err
	}
	if strings.ContainsAny(path, "\r\n\x00$`*?[") {
		return "", errors.New("managed snippet path contains characters unsupported by safe static repair")
	}
	escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(path)
	return "include \"" + escaped + "\"", nil
}

func appendConfigLine(content []byte, line string) []byte {
	result := make([]byte, 0, len(content)+len(line)+2)
	result = append(result, content...)
	if len(result) != 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	result = append(result, line...)
	result = append(result, '\n')
	return result
}

func readOptionalRepairFile(path string) ([]byte, safeFileState, bool, error) {
	content, state, err := readSafeConfigFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, safeFileState{}, false, nil
	}
	return content, state, err == nil, err
}

func inspectSafeRepairDirectory(path string) (safeDirectoryState, error) {
	if _, err := cleanAbsolutePath(path); err != nil {
		return safeDirectoryState{}, err
	}
	if err := validateRepairAncestors(path); err != nil {
		return safeDirectoryState{}, err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return safeDirectoryState{}, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return safeDirectoryState{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Getuid()) || stat.Mode&0o022 != 0 {
		return safeDirectoryState{}, errors.New("directory must be owned by the current user and not group- or world-writable")
	}
	return safeDirectoryState{
		device: uint64(stat.Dev), inode: stat.Ino, owner: stat.Uid,
		mode: os.FileMode(stat.Mode & 0o777),
	}, nil
}

func validateRepairAncestors(path string) error {
	uid := uint32(os.Getuid())
	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a real directory", current)
		}
		if stat.Uid != 0 && stat.Uid != uid {
			return fmt.Errorf("%s is owned by untrusted uid %d", current, stat.Uid)
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("%s is group- or world-writable without the sticky bit", current)
		}
	}
	return nil
}

func sameRepairPlan(left, right Plan) bool {
	if left.request != right.request || left.fixID != right.fixID || left.trusted != right.trusted || left.ID != right.ID || left.Summary != right.Summary || !slices.Equal(left.Changes, right.Changes) || len(left.edits) != len(right.edits) {
		return false
	}
	for index := range left.edits {
		a, b := left.edits[index], right.edits[index]
		if a.path != b.path || !bytes.Equal(a.oldContent, b.oldContent) || !bytes.Equal(a.newContent, b.newContent) ||
			a.oldState != b.oldState || a.existed != b.existed || a.directory != b.directory || a.mode != b.mode ||
			a.preview != b.preview ||
			!slices.Equal(a.observed, b.observed) {
			return false
		}
	}
	return true
}

func revalidateEdit(edit fileEdit) error {
	for _, observed := range edit.observed {
		content, state, err := readSafeConfigFile(observed.path)
		if err != nil {
			return fmt.Errorf("observed configuration cannot be revalidated: %w", err)
		}
		if state != observed.state || sha256.Sum256(content) != observed.digest {
			return errors.New("observed configuration changed since the preview")
		}
	}
	directory, err := inspectSafeRepairDirectory(filepath.Dir(edit.path))
	if err != nil {
		return err
	}
	if directory != edit.directory {
		return errors.New("parent directory was replaced or changed")
	}
	content, state, exists, err := readOptionalRepairFile(edit.path)
	if err != nil {
		return err
	}
	if exists != edit.existed {
		return errors.New("target existence changed")
	}
	if exists && (state != edit.oldState || !bytes.Equal(content, edit.oldContent)) {
		return errors.New("target was changed or replaced")
	}
	return nil
}

func createExclusiveBackup(edit fileEdit) (string, error) {
	directory, err := openVerifiedDirectory(filepath.Dir(edit.path), edit.directory)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	for range temporaryAttempts {
		suffix, err := randomSuffix()
		if err != nil {
			return "", err
		}
		name := filepath.Base(edit.path) + ".sway-session.bak-" + suffix
		fd, err := unix.Openat(int(directory.Fd()), name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		file := os.NewFile(uintptr(fd), name)
		writeErr := writeAndSync(file, edit.oldContent, 0o600)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = unix.Unlinkat(int(directory.Fd()), name, 0)
			return "", errors.Join(writeErr, closeErr)
		}
		if err := directory.Sync(); err != nil {
			return "", err
		}
		return filepath.Join(filepath.Dir(edit.path), name), nil
	}
	return "", errors.New("could not allocate a unique backup path")
}

func atomicWriteEdit(edit fileEdit) (appliedFileEdit, error) {
	if err := revalidateEdit(edit); err != nil {
		return appliedFileEdit{}, err
	}
	directory, err := openVerifiedDirectory(filepath.Dir(edit.path), edit.directory)
	if err != nil {
		return appliedFileEdit{}, err
	}
	defer directory.Close()
	name := filepath.Base(edit.path)
	for range temporaryAttempts {
		suffix, err := randomSuffix()
		if err != nil {
			return appliedFileEdit{}, err
		}
		temporary := "." + name + ".tmp-" + suffix
		fd, err := unix.Openat(int(directory.Fd()), temporary,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return appliedFileEdit{}, err
		}
		file := os.NewFile(uintptr(fd), temporary)
		writeErr := writeAndSync(file, edit.newContent, edit.mode)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = unix.Unlinkat(int(directory.Fd()), temporary, 0)
			return appliedFileEdit{}, errors.Join(writeErr, closeErr)
		}
		_, installed, err := readSafeConfigFileAt(int(directory.Fd()), temporary)
		if err != nil {
			_ = unix.Unlinkat(int(directory.Fd()), temporary, 0)
			return appliedFileEdit{}, err
		}
		flags := uint(unix.RENAME_NOREPLACE)
		if edit.existed {
			flags = unix.RENAME_EXCHANGE
		}
		if err := unix.Renameat2(int(directory.Fd()), temporary, int(directory.Fd()), name, flags); err != nil {
			_ = unix.Unlinkat(int(directory.Fd()), temporary, 0)
			return appliedFileEdit{}, err
		}
		receipt := appliedFileEdit{edit: edit, installed: installed}
		if edit.existed {
			displaced, state, err := readSafeConfigFileAt(int(directory.Fd()), temporary)
			if err != nil || state != edit.oldState || !bytes.Equal(displaced, edit.oldContent) {
				return appliedFileEdit{}, restoreDisplacedFile(directory, temporary, name, receipt)
			}
			if err := unix.Unlinkat(int(directory.Fd()), temporary, 0); err != nil {
				return receipt, fmt.Errorf("preserved displaced configuration at %s: %w", filepath.Join(filepath.Dir(edit.path), temporary), err)
			}
		}
		return receipt, directory.Sync()
	}
	return appliedFileEdit{}, errors.New("could not allocate a unique temporary path")
}

// Exchange preserves the displaced file until its identity and contents have
// been checked. An ordinary editor save during staging is restored; if another
// writer intervenes again, retain the displaced file and report its path.
func restoreDisplacedFile(directory *os.File, temporary, name string, receipt appliedFileEdit) error {
	path := filepath.Join(filepath.Dir(receipt.edit.path), temporary)
	current, state, err := readSafeConfigFileAt(int(directory.Fd()), name)
	if err != nil || state != receipt.installed || !bytes.Equal(current, receipt.edit.newContent) {
		return &unresolvedRepairError{fmt.Errorf("target changed during repair; preserved concurrent configuration at %s", path)}
	}
	if err := unix.Renameat2(int(directory.Fd()), temporary, int(directory.Fd()), name, unix.RENAME_EXCHANGE); err != nil {
		return &unresolvedRepairError{fmt.Errorf("target changed during repair; preserved concurrent configuration at %s: %w", path, err)}
	}
	current, state, err = readSafeConfigFileAt(int(directory.Fd()), temporary)
	if err != nil || state != receipt.installed || !bytes.Equal(current, receipt.edit.newContent) {
		return &unresolvedRepairError{fmt.Errorf("target changed again during repair; preserved additional concurrent configuration at %s", path)}
	}
	if err := unix.Unlinkat(int(directory.Fd()), temporary, 0); err != nil {
		return &unresolvedRepairError{fmt.Errorf("concurrent configuration restored; temporary repair file remains at %s: %w", path, err)}
	}
	if err := directory.Sync(); err != nil {
		return &unresolvedRepairError{fmt.Errorf("concurrent configuration restored but directory sync failed: %w", err)}
	}
	return errors.New("target changed during repair; concurrent configuration was restored")
}

func unlinkProvenFile(receipt appliedFileEdit) error {
	edit := receipt.edit
	directory, err := openVerifiedDirectory(filepath.Dir(edit.path), edit.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	// Move first, then verify, so a racing replacement is preserved instead of
	// being unlinked merely because it has identical bytes.
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	name := filepath.Base(edit.path)
	temporary := "." + name + ".rollback-" + suffix
	if err := unix.Renameat2(int(directory.Fd()), name, int(directory.Fd()), temporary, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	current, state, err := readSafeConfigFileAt(int(directory.Fd()), temporary)
	if err != nil || state != receipt.installed || !bytes.Equal(current, edit.newContent) {
		if err := unix.Renameat2(int(directory.Fd()), temporary, int(directory.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
			return fmt.Errorf("created target changed; preserved concurrent configuration at %s", filepath.Join(filepath.Dir(edit.path), temporary))
		}
		return errors.New("created target changed; concurrent configuration was restored")
	}
	if err := unix.Unlinkat(int(directory.Fd()), temporary, 0); err != nil {
		return fmt.Errorf("temporary repair file remains at %s: %w", filepath.Join(filepath.Dir(edit.path), temporary), err)
	}
	return directory.Sync()
}

func openVerifiedDirectory(path string, expected safeDirectoryState) (*os.File, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		directory.Close()
		return nil, err
	}
	current := safeDirectoryState{
		device: uint64(stat.Dev), inode: stat.Ino, owner: stat.Uid,
		mode: os.FileMode(stat.Mode & 0o777),
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || current != expected {
		directory.Close()
		return nil, errors.New("parent directory was replaced or changed")
	}
	return directory, nil
}

func writeAndSync(file *os.File, content []byte, mode os.FileMode) error {
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	return file.Sync()
}

func randomSuffix() (string, error) {
	var entropy [12]byte
	if _, err := io.ReadFull(rand.Reader, entropy[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(entropy[:]), nil
}

func nonemptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
