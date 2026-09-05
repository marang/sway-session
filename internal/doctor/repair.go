package doctor

import (
	"bytes"
	"context"
	"crypto/rand"
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
	path            string
	oldContent      []byte
	newContent      []byte
	oldState        safeFileState
	existed         bool
	directory       safeDirectoryState
	mode            os.FileMode
	preview         string
	missing         []integrationKind
	root            string
	snippetIncluded bool
}

type safeDirectoryState struct {
	device uint64
	inode  uint64
	owner  uint32
	mode   os.FileMode
}

func (service *Service) Plan(ctx context.Context, fixID string) (Plan, error) {
	if fixID != swayIntegrationFixID {
		return Plan{}, fmt.Errorf("unknown repair %q", fixID)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}

	analysis, err := analyzeSwayConfig(ctx, service.options)
	if err != nil {
		return Plan{}, err
	}
	if analysis.unsupported != nil {
		return Plan{}, fmt.Errorf("repair is unavailable: %w", analysis.unsupported)
	}
	missing, err := repairableMissing(analysis)
	if err != nil {
		return Plan{}, err
	}
	if len(missing) == 0 {
		return Plan{}, errors.New("sway integration repair is not needed")
	}

	rootContent, rootState, err := readSafeConfigFile(analysis.root)
	if err != nil {
		return Plan{}, fmt.Errorf("revalidate root Sway configuration: %w", err)
	}
	if rootState.owner != uint32(os.Getuid()) {
		return Plan{}, errors.New("repair requires the root Sway configuration to be owned by the current user")
	}
	rootDirectory, err := inspectSafeRepairDirectory(filepath.Dir(analysis.root))
	if err != nil {
		return Plan{}, fmt.Errorf("validate root Sway configuration directory: %w", err)
	}

	snippetPath := filepath.Join(filepath.Dir(analysis.root), doctorSnippetName)
	if snippetPath == analysis.root {
		return Plan{}, errors.New("root Sway configuration cannot be the doctor-managed snippet")
	}
	snippetDirectory := rootDirectory
	snippetContent, snippetState, snippetExists, err := readOptionalRepairFile(snippetPath)
	if err != nil {
		return Plan{}, fmt.Errorf("inspect doctor-managed snippet: %w", err)
	}
	if snippetExists && snippetState.owner != uint32(os.Getuid()) {
		return Plan{}, errors.New("doctor-managed snippet is not owned by the current user")
	}

	included := false
	if _, ok := analysis.includes[snippetPath]; ok {
		included = true
	}
	existingKinds := []integrationKind(nil)
	if snippetExists {
		existingKinds, err = parseManagedSnippet(snippetContent)
		if err != nil {
			return Plan{}, fmt.Errorf("refuse to update doctor-managed snippet: %w", err)
		}
	}
	desiredKinds := slices.Clone(missing)
	if included {
		for _, kind := range existingKinds {
			if !slices.Contains(desiredKinds, kind) {
				desiredKinds = append(desiredKinds, kind)
			}
		}
	}
	sortIntegrationKinds(desiredKinds)
	executable, err := repairExecutable(service.options.Executable)
	if err != nil {
		return Plan{}, err
	}
	newSnippet := renderManagedSnippet(executable, desiredKinds)

	common := fileEdit{
		missing:         slices.Clone(missing),
		root:            analysis.root,
		snippetIncluded: included,
	}
	plan := Plan{ID: swayIntegrationFixID, Summary: "Add missing sway-session integration directives through a doctor-managed snippet."}
	if !snippetExists || !bytes.Equal(snippetContent, newSnippet) {
		edit := common
		edit.path = snippetPath
		edit.oldContent = slices.Clone(snippetContent)
		edit.newContent = newSnippet
		edit.oldState = snippetState
		edit.existed = snippetExists
		edit.directory = snippetDirectory
		edit.mode = 0o600
		if snippetExists {
			edit.mode = snippetState.mode
		}
		edit.preview = string(newSnippet)
		plan.edits = append(plan.edits, edit)
		plan.Changes = append(plan.Changes, FileChange{Path: snippetPath, Preview: string(newSnippet)})
	}
	if !included {
		includeLine, err := renderIncludeLine(snippetPath)
		if err != nil {
			return Plan{}, err
		}
		newRoot := appendConfigLine(rootContent, includeLine)
		edit := common
		edit.path = analysis.root
		edit.oldContent = slices.Clone(rootContent)
		edit.newContent = newRoot
		edit.oldState = rootState
		edit.existed = true
		edit.directory = rootDirectory
		edit.mode = rootState.mode
		edit.preview = "+ " + includeLine + "\n"
		plan.edits = append(plan.edits, edit)
		plan.Changes = append(plan.Changes, FileChange{Path: analysis.root, Preview: edit.preview})
	}
	if len(plan.edits) == 0 {
		return Plan{}, errors.New("sway integration repair produced no safe changes")
	}
	return plan, nil
}

func (service *Service) Apply(ctx context.Context, plan Plan) (FixResult, error) {
	if plan.ID != swayIntegrationFixID || len(plan.edits) == 0 {
		return FixResult{}, errors.New("repair plan is missing trusted private edit data")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return FixResult{}, err
	}
	fresh, err := service.Plan(ctx, plan.ID)
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

	applied := 0
	for index, edit := range plan.edits {
		if err := ctx.Err(); err != nil {
			return FixResult{}, service.rollbackFailure(plan.edits[:applied], backups, err)
		}
		if err := atomicWriteEdit(edit); err != nil {
			return FixResult{}, service.rollbackFailure(plan.edits[:index+1], backups,
				fmt.Errorf("apply %s: %w", edit.path, err))
		}
		applied = index + 1
	}

	resultBackups := make([]string, 0, len(backups))
	for _, backup := range backups {
		if backup != "" {
			resultBackups = append(resultBackups, backup)
		}
	}
	return FixResult{
		ID:      swayIntegrationFixID,
		Message: "Applied the managed Sway integration files. Reload Sway when convenient.",
		Backups: resultBackups,
	}, nil
}

func (service *Service) rollbackFailure(applied []fileEdit, backups []string, cause error) error {
	uncertain := false
	for index := len(applied) - 1; index >= 0; index-- {
		edit := applied[index]
		current, _, exists, err := readOptionalRepairFile(edit.path)
		if err != nil {
			uncertain = true
			continue
		}
		if edit.existed && exists && bytes.Equal(current, edit.oldContent) {
			continue
		}
		if !edit.existed && !exists {
			continue
		}
		if !exists || !bytes.Equal(current, edit.newContent) {
			uncertain = true
			continue
		}
		if edit.existed {
			restore := edit
			restore.newContent = edit.oldContent
			restore.mode = edit.oldState.mode
			if err := atomicWriteEditWithoutOldIdentity(restore); err != nil {
				uncertain = true
			}
			continue
		}
		if err := unlinkProvenFile(edit); err != nil {
			uncertain = true
		}
	}
	hints := nonemptyStrings(backups)
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

func repairableMissing(analysis swayConfigAnalysis) ([]integrationKind, error) {
	missing := make([]integrationKind, 0, len(integrationOrder))
	for _, kind := range integrationOrder {
		occurrences := analysis.occurrences[kind]
		correct := 0
		for _, occurrence := range occurrences {
			if occurrence.correct {
				correct++
			}
		}
		if len(occurrences) == 0 {
			missing = append(missing, kind)
			continue
		}
		if len(occurrences) != 1 || correct != 1 {
			return nil, fmt.Errorf("repair is unavailable because %s is duplicated or conflicting", integrationLabel(kind))
		}
	}
	return missing, nil
}

func repairSafetyAvailable(analysis swayConfigAnalysis, options Options) error {
	_, rootState, err := readSafeConfigFile(analysis.root)
	if err != nil {
		return fmt.Errorf("root configuration is not safe to update: %w", err)
	}
	if rootState.owner != uint32(os.Getuid()) {
		return errors.New("root configuration is not owned by the current user")
	}
	if _, err := inspectSafeRepairDirectory(filepath.Dir(analysis.root)); err != nil {
		return fmt.Errorf("configuration directory is not safe to update: %w", err)
	}
	snippetPath := filepath.Join(filepath.Dir(analysis.root), doctorSnippetName)
	if snippetPath == analysis.root {
		return errors.New("root configuration path conflicts with the managed snippet path")
	}
	content, state, exists, err := readOptionalRepairFile(snippetPath)
	if err != nil {
		return fmt.Errorf("managed snippet is not safe to update: %w", err)
	}
	if exists {
		if state.owner != uint32(os.Getuid()) {
			return errors.New("managed snippet is not owned by the current user")
		}
		if _, err := parseManagedSnippet(content); err != nil {
			return err
		}
	}
	_, err = repairExecutable(options.Executable)
	return err
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

func parseManagedSnippet(content []byte) ([]integrationKind, error) {
	if !bytes.HasPrefix(content, []byte(doctorHeader)) {
		return nil, errors.New("file does not have the exact doctor ownership header")
	}
	remainder := strings.TrimSuffix(string(content[len(doctorHeader):]), "\n")
	if remainder == "" {
		return nil, nil
	}
	lines := strings.Split(remainder, "\n")
	kinds := make([]integrationKind, 0, len(lines))
	executable := ""
	for _, line := range lines {
		tokens, unsupported, err := tokenizeSwayLine(line)
		if err != nil || unsupported || len(tokens) == 0 {
			return nil, errors.New("file contains unrecognized manual edits")
		}
		kind, lineExecutable, ok := parseExactManagedDirective(tokens)
		if !ok {
			return nil, errors.New("file contains unrecognized manual edits")
		}
		if executable != "" && executable != lineExecutable {
			return nil, errors.New("file contains inconsistent managed executable paths")
		}
		executable = lineExecutable
		if slices.Contains(kinds, kind) {
			return nil, errors.New("file contains duplicate managed directives")
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
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

func sortIntegrationKinds(kinds []integrationKind) {
	slices.SortFunc(kinds, func(left, right integrationKind) int { return int(left) - int(right) })
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
	if left.ID != right.ID || left.Summary != right.Summary || !slices.Equal(left.Changes, right.Changes) || len(left.edits) != len(right.edits) {
		return false
	}
	for index := range left.edits {
		a, b := left.edits[index], right.edits[index]
		if a.path != b.path || !bytes.Equal(a.oldContent, b.oldContent) || !bytes.Equal(a.newContent, b.newContent) ||
			a.oldState != b.oldState || a.existed != b.existed || a.directory != b.directory || a.mode != b.mode ||
			a.preview != b.preview || a.root != b.root || a.snippetIncluded != b.snippetIncluded || !slices.Equal(a.missing, b.missing) {
			return false
		}
	}
	return true
}

func revalidateEdit(edit fileEdit) error {
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

func atomicWriteEdit(edit fileEdit) error {
	if err := revalidateEdit(edit); err != nil {
		return err
	}
	return atomicWriteEditWithoutOldIdentity(edit)
}

func atomicWriteEditWithoutOldIdentity(edit fileEdit) error {
	directory, err := openVerifiedDirectory(filepath.Dir(edit.path), edit.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	name := filepath.Base(edit.path)
	for range temporaryAttempts {
		suffix, err := randomSuffix()
		if err != nil {
			return err
		}
		temporary := "." + name + ".tmp-" + suffix
		fd, err := unix.Openat(int(directory.Fd()), temporary,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), temporary)
		writeErr := writeAndSync(file, edit.newContent, edit.mode)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = unix.Unlinkat(int(directory.Fd()), temporary, 0)
			return errors.Join(writeErr, closeErr)
		}
		if err := unix.Renameat(int(directory.Fd()), temporary, int(directory.Fd()), name); err != nil {
			_ = unix.Unlinkat(int(directory.Fd()), temporary, 0)
			return err
		}
		return directory.Sync()
	}
	return errors.New("could not allocate a unique temporary path")
}

func unlinkProvenFile(edit fileEdit) error {
	directory, err := openVerifiedDirectory(filepath.Dir(edit.path), edit.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	current, _, exists, err := readOptionalRepairFile(edit.path)
	if err != nil || !exists || !bytes.Equal(current, edit.newContent) {
		return errors.New("created target can no longer be proven to be doctor-owned")
	}
	if err := unix.Unlinkat(int(directory.Fd()), filepath.Base(edit.path), 0); err != nil {
		return err
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
